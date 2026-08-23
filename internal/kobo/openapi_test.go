package kobo_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/kobo"
)

// The document is embedded, so a typo in it is a page that fails to draw rather
// than a build that fails to compile. This is what makes it the latter.
func TestOpenAPIParsesAndCoversWhatWeServe(t *testing.T) {
	raw, err := kobo.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}

	var spec struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]map[string]struct {
			Tags         []string `json:"tags"`
			OperationID  string   `json:"operationId"`
			Kind         string   `json:"x-kobibri"`
			Evidence     string   `json:"x-kobibri-evidence"`
			EvidenceNote string   `json:"x-kobibri-evidence-note"`
			Responses    map[string]any
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("the document is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.") {
		t.Errorf("openapi = %q, want a 3.x document", spec.OpenAPI)
	}

	// Every route the router registers has to be described. A path added
	// without a line here is an endpoint nobody can find out about.
	for _, path := range []string{
		"/v1/initialization",
		"/v1/auth/device",
		"/v1/auth/refresh",
		"/v1/library/sync",
		"/v1/library/{uuid}/metadata",
		"/v1/library/{uuid}/state",
		"/v1/library/{uuid}",
		"/v1/library/tags",
		"/v1/library/tags/{id}",
		"/v1/library/tags/{id}/items",
		"/v1/library/tags/{id}/items/delete",
		"/download/{uuid}/{format}",
		"/covers/{imageId}/{width}/{height}/{isGreyscale}/image.jpg",
		"/covers/{imageId}/{width}/{height}/{quality}/{isGreyscale}/image.jpg",
		"/v1/analytics/event",
		"/v1/analytics/gettests",
		"/v1/user/profile",
		"/v1/user/wishlist",
		"/v1/user/loyalty/benefits",
		"/v1/deals",
		"/v1/affiliate",
	} {
		if _, ok := spec.Paths[path]; !ok {
			t.Errorf("%s is served but not documented", path)
		}
	}

	ids := map[string]string{}
	kinds := map[string]int{}
	evidence := map[string]int{}
	for path, item := range spec.Paths {
		for method, op := range item {
			where := strings.ToUpper(method) + " " + path
			if len(op.Responses) == 0 {
				t.Errorf("%s documents no response", where)
			}
			if op.OperationID == "" {
				t.Errorf("%s has no operationId", where)
			} else if prev, clash := ids[op.OperationID]; clash {
				t.Errorf("operationId %q is used by both %s and %s", op.OperationID, prev, where)
			} else {
				ids[op.OperationID] = where
			}
			switch op.Kind {
			case "served", "relayed", "proxyable":
				kinds[op.Kind]++
			default:
				t.Errorf("%s has x-kobibri = %q, want served, relayed or proxyable", where, op.Kind)
			}
			if len(op.Tags) == 0 {
				t.Errorf("%s has no tag, so it lands in no section of the page", where)
			}
			// What we do with an endpoint and how much we know about it are two
			// questions, and the second is the one that goes stale silently.
			switch op.Evidence {
			case "observed", "implemented", "unproven":
				evidence[op.Evidence]++
			default:
				t.Errorf("%s has x-kobibri-evidence = %q, want observed, implemented or unproven", where, op.Evidence)
			}
			if op.EvidenceNote == "" {
				t.Errorf("%s claims %q without saying on what grounds", where, op.Evidence)
			}
		}
	}

	// The proxyable half is derived from the resource map rather than written
	// out. A handful would mean the derivation quietly stopped working.
	if kinds["proxyable"] < 40 {
		t.Errorf("only %d proxyable paths came out of the resource map, expected dozens", kinds["proxyable"])
	}
	if kinds["served"] == 0 || kinds["relayed"] == 0 {
		t.Errorf("kinds = %v, want some of each", kinds)
	}
	for _, level := range []string{"observed", "implemented", "unproven"} {
		if evidence[level] == 0 {
			t.Errorf("nothing is marked %q; the axis has collapsed to one value", level)
		}
	}

	// The two axes have to be independent, or one of them is decoration. A
	// served endpoint nobody has watched work is exactly the case worth seeing.
	if evidence["observed"] >= kinds["served"]+kinds["relayed"] {
		t.Error("everything served or relayed is marked observed, which is not what the traces show")
	}
}

// Everything the page draws comes through here, so a document that parses but
// renders empty is worth catching separately.
func TestOpenAPIRenderModel(t *testing.T) {
	doc, err := kobo.ParseOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title == "" || doc.Description == "" {
		t.Error("the document has no title or no description")
	}

	total := 0
	for _, g := range doc.Groups {
		if g.Name == "" || g.Description == "" {
			t.Errorf("group %+v is missing a name or a description", g)
		}
		total += len(g.Operations)
		for _, op := range g.Operations {
			if op.Method == "" || op.Path == "" || op.Summary == "" {
				t.Errorf("%+v is missing a method, a path or a summary", op)
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s %s renders with no responses", op.Method, op.Path)
			}
		}
	}
	if total < 60 {
		t.Errorf("only %d operations reached the page", total)
	}

	// A request body drawn as a media type and nothing else says less than the
	// shape does. Every documented payload has to render as readable JSON.
	var bodies int
	for _, g := range doc.Groups {
		for _, op := range g.Operations {
			if op.RequestBody != nil {
				bodies++
				if op.RequestBody.MediaType == "" || op.RequestBody.Example == "" {
					t.Errorf("%s %s has a request body that renders as nothing", op.Method, op.Path)
				}
			}
			for _, r := range op.Responses {
				if r.Body != nil && r.Body.Example == "" && r.Body.MediaType == "application/json" {
					t.Errorf("%s %s %s has a JSON response that renders as nothing", op.Method, op.Path, r.Status)
				}
			}
			if op.Evidence == "" || op.EvidenceNote == "" {
				t.Errorf("%s %s reaches the page with no evidence marking", op.Method, op.Path)
			}
		}
	}
	if bodies < 4 {
		t.Errorf("only %d request bodies rendered; the sampler is not resolving schemas", bodies)
	}

	// A $ref parameter has to be resolved on the way in, or the page shows a
	// row with no name.
	var seen bool
	for _, g := range doc.Groups {
		for _, op := range g.Operations {
			for _, p := range op.Parameters {
				if p.Name == "" {
					t.Errorf("%s %s has a parameter with no name; a $ref went unresolved", op.Method, op.Path)
				}
				if p.Name == "uuid" {
					seen = true
				}
			}
		}
	}
	if !seen {
		t.Error("the shared uuid parameter never resolved")
	}
}
