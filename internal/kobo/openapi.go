package kobo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed openapi.json
var openAPICore []byte

// storePrefix is the host every proxyable path in the resource map hangs off.
const storePrefix = "https://storeapi.kobo.com"

// OpenAPI returns the specification as JSON: the hand-written core, plus every
// store path the resource map names.
//
// The second half is derived rather than written down. `nativeResources` is a
// real map captured from the store, so walking it is what makes the proxyable
// surface complete and keeps it in step: a key added to the map turns up in the
// documentation without anyone remembering to add it.
func OpenAPI() ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(openAPICore, &doc); err != nil {
		return nil, fmt.Errorf("the built-in OpenAPI document does not parse: %w", err)
	}

	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		return nil, fmt.Errorf("the built-in OpenAPI document has no paths")
	}
	for path, op := range proxyablePaths(paths) {
		paths[path] = op
	}

	if tags, ok := doc["tags"].([]any); ok {
		doc["tags"] = append(tags, map[string]any{
			"name": "Proxyable",
			"description": "Named in Kobo's own resource map and relayed if a device ever asks for " +
				"them. kobibri has no answer of its own here, so there are no schemas: " +
				"writing down a response nobody has observed would be inventing it. " +
				"Derived from the built-in resource map, so this list is exactly as " +
				"complete as that map is.",
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

// observedProxies are the relayed endpoints a real device has actually been
// caught asking for. Everything else in the proxyable half is a path named in
// Kobo's map that nothing has ever called here, and saying so is the point.
var observedProxies = map[string]string{
	"product_prices":   "Kobo Libra Colour, fw 4.45.23697, 2026-08-24. Relayed; the store answered 200 {\"Items\":[]}.",
	"product_nextread": "Kobo Libra Colour, fw 4.45.23697, 2026-08-24. Relayed; the store answered 200.",
}

// proxyablePaths turns the store URLs in the resource map into path items,
// skipping anything the core document already describes.
func proxyablePaths(known map[string]any) map[string]any {
	byPath := map[string]string{}
	for key, value := range nativeResources {
		url, ok := value.(string)
		if !ok || !strings.HasPrefix(url, storePrefix) {
			continue
		}
		path, _, _ := strings.Cut(strings.TrimPrefix(url, storePrefix), "?")
		if path == "" || path == "/" {
			continue
		}
		if _, already := known[path]; already {
			continue
		}
		// Two keys can name one path — delete_tag and rename_tag both do. Keep
		// the first alphabetically so the output does not depend on map order.
		if prev, clash := byPath[path]; !clash || key < prev {
			byPath[path] = key
		}
	}

	out := make(map[string]any, len(byPath))
	for path, key := range byPath {
		evidence, note := "unproven", "Named in Kobo's resource map. No device has been seen asking for it here, so there is nothing to say about its shape that would not be invention."
		if seen, ok := observedProxies[key]; ok {
			evidence, note = "observed", seen
		}
		out[path] = map[string]any{
			"get": map[string]any{
				"tags":        []any{"Proxyable"},
				"summary":     "Relayed to the store",
				"operationId": "proxy_" + key,
				"description": "Resource-map key `" + key + "`.\n\nRelayed to `" + storePrefix + path +
					"` with the device's own headers, unaltered. A response under 400 is copied " +
					"back verbatim; anything from 400 up is logged with its body and answered " +
					"`200 {}` here instead, because a 4xx from this host aborts the reader's " +
					"whole sync.\n\nGET is what the documentation says because it is the only " +
					"method observed on endpoints of this kind. Every method is relayed all the " +
					"same — the proxy does not care which.",
				"x-kobibri":               "proxyable",
				"x-kobibri-evidence":      evidence,
				"x-kobibri-evidence-note": note,
				"x-kobo-resource":         key,
				"responses": map[string]any{
					"200": map[string]any{"description": "Whatever the store said, or `{}` if it would not say it."},
				},
			},
		}
	}
	return out
}

// Rendering model. It is deliberately a subset: enough to draw the page, and no
// more, so the document stays the source of truth rather than this.

type APIDoc struct {
	Title       string
	Version     string
	Summary     string
	Description string
	Groups      []APIGroup
}

type APIGroup struct {
	Name        string
	Description string
	Operations  []APIOperation
}

type APIOperation struct {
	Method      string
	Path        string
	Summary     string
	Description string
	// Kind is served, relayed or proxyable — what this server does with it.
	Kind string
	// Evidence is observed, implemented or unproven — how much we actually know.
	// The two are separate questions: an endpoint can be served and never seen,
	// or relayed and watched working every few minutes.
	Evidence     string
	EvidenceNote string
	Parameters   []APIParam
	RequestBody  *APIBody
	Responses    []APIResponse
}

// APIBody is a request or response payload, rendered as the JSON it looks like
// rather than as a schema tree. A shape someone can read beats a shape someone
// has to assemble in their head.
type APIBody struct {
	MediaType string
	Example   string
}

type APIParam struct {
	Name        string
	In          string
	Required    bool
	Type        string
	Description string
}

type APIResponse struct {
	Status      string
	Description string
	Body        *APIBody
}

// mediaType is one entry under content: the schema, and an example if the
// document carries a better one than the schema can produce.
type mediaType struct {
	Schema  map[string]any `json:"schema"`
	Example any            `json:"example"`
}

// sample renders the payload as the JSON it looks like on the wire.
func (m mediaType) sample(doc map[string]any) string {
	value := m.Example
	if value == nil {
		if m.Schema == nil {
			return ""
		}
		value = sampleOf(doc, m.Schema, 0)
	}
	if value == nil {
		return ""
	}
	buf, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ""
	}
	return string(buf)
}

// sampleOf builds a representative value from a schema, resolving $ref against
// the document's components.
//
// The depth cap is what keeps a self-referential schema from running away; the
// document has none today, and relying on that is how it would break later.
func sampleOf(doc map[string]any, schema map[string]any, depth int) any {
	if schema == nil || depth > 6 {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok {
		return sampleOf(doc, resolveRef(doc, ref), depth+1)
	}
	if example, ok := schema["example"]; ok {
		return example
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}

	switch schema["type"] {
	case "object":
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			return map[string]any{}
		}
		out := make(map[string]any, len(props))
		for name, raw := range props {
			field, _ := raw.(map[string]any)
			out[name] = sampleOf(doc, field, depth+1)
		}
		return out
	case "array":
		items, _ := schema["items"].(map[string]any)
		one := sampleOf(doc, items, depth+1)
		if one == nil {
			return []any{}
		}
		return []any{one}
	case "boolean":
		return false
	case "integer":
		return 0
	case "number":
		return 0
	case "string":
		switch schema["format"] {
		case "uuid":
			return "00000000-0000-0000-0000-000000000000"
		case "date-time":
			return "2026-08-24T00:00:00Z"
		case "uri":
			return "https://books.example.com/…"
		case "binary":
			return "<binary>"
		}
		return "string"
	}
	return nil
}

func resolveRef(doc map[string]any, ref string) map[string]any {
	const prefix = "#/"
	if !strings.HasPrefix(ref, prefix) {
		return nil
	}
	node := any(doc)
	for _, step := range strings.Split(strings.TrimPrefix(ref, prefix), "/") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[step]
	}
	out, _ := node.(map[string]any)
	return out
}

// ParseOpenAPI reads the specification into the shape the documentation page
// draws. Failing here is a broken build rather than a broken request, which is
// why the caller is expected to do it once at startup.
func ParseOpenAPI() (*APIDoc, error) {
	raw, err := OpenAPI()
	if err != nil {
		return nil, err
	}

	var spec struct {
		Info struct {
			Title       string `json:"title"`
			Version     string `json:"version"`
			Summary     string `json:"summary"`
			Description string `json:"description"`
		} `json:"info"`
		Tags []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tags"`
		Paths map[string]map[string]struct {
			Tags         []string `json:"tags"`
			Summary      string   `json:"summary"`
			Description  string   `json:"description"`
			Kind         string   `json:"x-kobibri"`
			Evidence     string   `json:"x-kobibri-evidence"`
			EvidenceNote string   `json:"x-kobibri-evidence-note"`
			Parameters   []struct {
				Ref         string `json:"$ref"`
				Name        string `json:"name"`
				In          string `json:"in"`
				Required    bool   `json:"required"`
				Description string `json:"description"`
				Schema      struct {
					Type string `json:"type"`
				} `json:"schema"`
			} `json:"parameters"`
			RequestBody *struct {
				Content map[string]mediaType `json:"content"`
			} `json:"requestBody"`
			Responses map[string]struct {
				Description string               `json:"description"`
				Content     map[string]mediaType `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
		Components struct {
			Parameters map[string]struct {
				Name        string `json:"name"`
				In          string `json:"in"`
				Required    bool   `json:"required"`
				Description string `json:"description"`
				Schema      struct {
					Type string `json:"type"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}

	// A second pass as plain maps, only so $ref can be followed when building
	// the examples. The typed struct above stays the shape the page draws.
	var whole map[string]any
	if err := json.Unmarshal(raw, &whole); err != nil {
		return nil, err
	}

	doc := &APIDoc{
		Title:       spec.Info.Title,
		Version:     spec.Info.Version,
		Summary:     spec.Info.Summary,
		Description: spec.Info.Description,
	}

	for _, t := range spec.Tags {
		doc.Groups = append(doc.Groups, APIGroup{Name: t.Name, Description: t.Description})
	}

	methods := []string{"get", "post", "put", "delete", "patch"}
	for path, item := range spec.Paths {
		for _, method := range methods {
			op, ok := item[method]
			if !ok {
				continue
			}

			out := APIOperation{
				Method:       strings.ToUpper(method),
				Path:         path,
				Summary:      op.Summary,
				Description:  op.Description,
				Kind:         op.Kind,
				Evidence:     op.Evidence,
				EvidenceNote: op.EvidenceNote,
			}
			for _, p := range op.Parameters {
				if name := strings.TrimPrefix(p.Ref, "#/components/parameters/"); p.Ref != "" {
					if shared, ok := spec.Components.Parameters[name]; ok {
						out.Parameters = append(out.Parameters, APIParam{
							Name: shared.Name, In: shared.In, Required: shared.Required,
							Type: shared.Schema.Type, Description: shared.Description,
						})
					}
					continue
				}
				out.Parameters = append(out.Parameters, APIParam{
					Name: p.Name, In: p.In, Required: p.Required,
					Type: p.Schema.Type, Description: p.Description,
				})
			}
			if op.RequestBody != nil {
				out.RequestBody = firstBody(whole, op.RequestBody.Content)
			}
			for status, r := range op.Responses {
				out.Responses = append(out.Responses, APIResponse{
					Status:      status,
					Description: r.Description,
					Body:        firstBody(whole, r.Content),
				})
			}
			sort.Slice(out.Responses, func(i, j int) bool { return out.Responses[i].Status < out.Responses[j].Status })

			group := "Proxyable"
			if len(op.Tags) > 0 {
				group = op.Tags[0]
			}
			for i := range doc.Groups {
				if doc.Groups[i].Name == group {
					doc.Groups[i].Operations = append(doc.Groups[i].Operations, out)
					break
				}
			}
		}
	}

	for i := range doc.Groups {
		ops := doc.Groups[i].Operations
		sort.Slice(ops, func(a, b int) bool {
			if ops[a].Path != ops[b].Path {
				return ops[a].Path < ops[b].Path
			}
			return ops[a].Method < ops[b].Method
		})
	}
	return doc, nil
}

// firstBody picks the one media type each payload in this document has. Sorted
// so a second one, if it ever appears, does not make the page flicker.
func firstBody(doc map[string]any, content map[string]mediaType) *APIBody {
	if len(content) == 0 {
		return nil
	}
	names := make([]string, 0, len(content))
	for name := range content {
		names = append(names, name)
	}
	sort.Strings(names)

	name := names[0]
	return &APIBody{MediaType: name, Example: content[name].sample(doc)}
}
