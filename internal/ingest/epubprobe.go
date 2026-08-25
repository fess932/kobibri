package ingest

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/fess932/kobibri/internal/store"
)

// EPUBInfo is what a probe learns about a book's layout.
type EPUBInfo struct {
	Version string
	Layout  string // store.LayoutReflowable or store.LayoutPrePaginated
}

// probeLimit bounds how much of an OPF we will read. Manifests can be large in
// image-heavy books, and we only need the metadata and spine.
const probeLimit = 4 << 20

// Probe inspects an EPUB to decide whether it is fixed layout.
//
// This matters because a pre-paginated book must be offered as EPUB3FL and must
// NOT be converted to KEPUB: it already has one chapter per page, which is
// enough for progress tracking, and the device renders it full screen. Running
// it through the converter would break that. See docs/NOTES.md.
//
// Only the container and the OPF are read — two small entries out of the zip's
// central directory.
func Probe(epubPath string) (EPUBInfo, error) {
	zr, err := zip.OpenReader(epubPath)
	if err != nil {
		return EPUBInfo{}, fmt.Errorf("open epub: %w", err)
	}
	defer zr.Close()

	opfPath, err := rootfilePath(zr)
	if err != nil {
		return EPUBInfo{}, err
	}

	f, err := zr.Open(opfPath)
	if err != nil {
		return EPUBInfo{}, fmt.Errorf("open %s: %w", opfPath, err)
	}
	defer f.Close()

	return parseOPF(io.LimitReader(f, probeLimit))
}

func rootfilePath(zr *zip.ReadCloser) (string, error) {
	f, err := zr.Open("META-INF/container.xml")
	if err != nil {
		return "", fmt.Errorf("epub has no META-INF/container.xml: %w", err)
	}
	defer f.Close()

	var container struct {
		Rootfiles []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xml.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&container); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	if len(container.Rootfiles) == 0 || container.Rootfiles[0].FullPath == "" {
		return "", fmt.Errorf("container.xml names no rootfile")
	}
	return path.Clean(container.Rootfiles[0].FullPath), nil
}

// parseOPF streams the package document, stopping once the spine is done.
//
// Fixed layout is declared in several ways depending on the tool that produced
// the book, so all of the shapes seen in the wild are accepted.
func parseOPF(r io.Reader) (EPUBInfo, error) {
	info := EPUBInfo{Layout: store.LayoutReflowable}
	dec := xml.NewDecoder(r)
	dec.Strict = false

	var (
		inMetadata      bool
		inLayoutMeta    bool
		layoutText      strings.Builder
		itemrefs        int
		prePaginatedRef int
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A malformed OPF is not fatal: default to reflowable, which is the
			// safe answer — a reflowable book converted to KEPUB still reads.
			break
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch strings.ToLower(t.Name.Local) {
			case "package":
				info.Version = attr(t, "version")
			case "metadata":
				inMetadata = true
			case "meta":
				if !inMetadata {
					break
				}
				property := strings.ToLower(attr(t, "property"))
				name := strings.ToLower(attr(t, "name"))
				content := strings.ToLower(attr(t, "content"))

				// EPUB 3: <meta property="rendition:layout">pre-paginated</meta>
				if property == "rendition:layout" {
					inLayoutMeta = true
					layoutText.Reset()
				}
				// Legacy and vendor-specific declarations.
				if name == "fixed-layout" && content == "true" {
					info.Layout = store.LayoutPrePaginated
				}
				if name == "original-resolution" || strings.HasPrefix(name, "com.apple.ibooks.display-options") {
					info.Layout = store.LayoutPrePaginated
				}
				if name == "rendition:layout" && content == "pre-paginated" {
					info.Layout = store.LayoutPrePaginated
				}
			case "itemref":
				itemrefs++
				if strings.Contains(strings.ToLower(attr(t, "properties")), "rendition:layout-pre-paginated") {
					prePaginatedRef++
				}
			}

		case xml.CharData:
			if inLayoutMeta {
				layoutText.Write(t)
			}

		case xml.EndElement:
			switch strings.ToLower(t.Name.Local) {
			case "metadata":
				inMetadata = false
			case "meta":
				if inLayoutMeta {
					if strings.TrimSpace(strings.ToLower(layoutText.String())) == "pre-paginated" {
						info.Layout = store.LayoutPrePaginated
					}
					inLayoutMeta = false
				}
			case "spine":
				// Nothing after the spine can change the answer.
				if itemrefs > 0 && float64(prePaginatedRef)/float64(itemrefs) >= 0.8 {
					info.Layout = store.LayoutPrePaginated
				}
				return info, nil
			}
		}
	}

	if itemrefs > 0 && float64(prePaginatedRef)/float64(itemrefs) >= 0.8 {
		info.Layout = store.LayoutPrePaginated
	}
	return info, nil
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}
