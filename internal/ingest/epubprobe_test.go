package ingest_test

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// writeEPUB builds an EPUB whose package document is exactly opf.
func writeEPUB(t *testing.T, opf string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.epub")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	write := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("mimetype", "application/epub+zip")
	write("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)
	write("OEBPS/content.opf", opf)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func opf(version, metadata, spine string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="` + version + `" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Probe</dc:title>` + metadata + `
  </metadata>
  <manifest><item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine>` + spine + `</spine>
</package>`
}

func TestProbeDetectsLayout(t *testing.T) {
	tests := []struct {
		name    string
		opf     string
		want    string
		wantVer string
	}{
		{
			name: "plain epub3 is reflowable",
			opf:  opf("3.0", "", `<itemref idref="c1"/>`),
			want: store.LayoutReflowable, wantVer: "3.0",
		},
		{
			name: "epub2 is reflowable",
			opf:  opf("2.0", "", `<itemref idref="c1"/>`),
			want: store.LayoutReflowable, wantVer: "2.0",
		},
		{
			name: "epub3 rendition:layout property",
			opf: opf("3.0", `<meta property="rendition:layout">pre-paginated</meta>`,
				`<itemref idref="c1"/>`),
			want: store.LayoutPrePaginated, wantVer: "3.0",
		},
		{
			name: "rendition:layout with surrounding whitespace",
			opf: opf("3.0", `<meta property="rendition:layout">
			  pre-paginated
			</meta>`, `<itemref idref="c1"/>`),
			want: store.LayoutPrePaginated, wantVer: "3.0",
		},
		{
			name: "legacy fixed-layout meta",
			opf:  opf("2.0", `<meta name="fixed-layout" content="true"/>`, `<itemref idref="c1"/>`),
			want: store.LayoutPrePaginated, wantVer: "2.0",
		},
		{
			name: "original-resolution meta",
			opf:  opf("2.0", `<meta name="original-resolution" content="1200x1600"/>`, `<itemref idref="c1"/>`),
			want: store.LayoutPrePaginated, wantVer: "2.0",
		},
		{
			name: "rendition:layout as a name/content pair",
			opf:  opf("3.0", `<meta name="rendition:layout" content="pre-paginated"/>`, `<itemref idref="c1"/>`),
			want: store.LayoutPrePaginated, wantVer: "3.0",
		},
		{
			name: "spine itemrefs carry the property",
			opf: opf("3.0", "", `<itemref idref="c1" properties="rendition:layout-pre-paginated"/>
			  <itemref idref="c2" properties="rendition:layout-pre-paginated"/>`),
			want: store.LayoutPrePaginated, wantVer: "3.0",
		},
		{
			name: "a single pre-paginated page among many is not fixed layout",
			opf: opf("3.0", "", `<itemref idref="c1" properties="rendition:layout-pre-paginated"/>
			  <itemref idref="c2"/><itemref idref="c3"/><itemref idref="c4"/><itemref idref="c5"/>`),
			want: store.LayoutReflowable, wantVer: "3.0",
		},
		{
			name: "reflowable override on a fixed-layout book still reads as fixed",
			opf: opf("3.0", `<meta property="rendition:layout">pre-paginated</meta>`,
				`<itemref idref="c1" properties="rendition:layout-reflowable"/>`),
			want: store.LayoutPrePaginated, wantVer: "3.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ingest.Probe(writeEPUB(t, tt.opf))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if info.Layout != tt.want {
				t.Errorf("Layout = %q, want %q", info.Layout, tt.want)
			}
			if info.Version != tt.wantVer {
				t.Errorf("Version = %q, want %q", info.Version, tt.wantVer)
			}
		})
	}
}

// A book we cannot read must produce an error rather than a wrong answer: the
// caller leaves it unprobed and offers it as KEPUB.
func TestProbeRejectsUnreadableFiles(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "broken.epub")
	if err := os.WriteFile(broken, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Probe(broken); err == nil {
		t.Error("probing a file that is not a zip succeeded")
	}

	noContainer := filepath.Join(t.TempDir(), "nocontainer.epub")
	f, err := os.Create(noContainer)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("mimetype")
	_, _ = w.Write([]byte("application/epub+zip"))
	_ = zw.Close()
	_ = f.Close()

	if _, err := ingest.Probe(noContainer); err == nil {
		t.Error("probing an epub with no container.xml succeeded")
	}
}

// A malformed package document must default to reflowable, which is the safe
// answer: a reflowable book converted to KEPUB still reads correctly.
func TestProbeDefaultsToReflowableOnMalformedOPF(t *testing.T) {
	path := writeEPUB(t, `<?xml version="1.0"?><package version="3.0"><metadata><dc:title>Broken`)

	info, err := ingest.Probe(path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Layout != store.LayoutReflowable {
		t.Errorf("Layout = %q, want %q for a truncated OPF", info.Layout, store.LayoutReflowable)
	}
}
