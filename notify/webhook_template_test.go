package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks in the **capability boundary** of the generic webhook template.
//
// This is the only place in the whole package where "a user-supplied string is evaluated as code", so it has to be
// clear what it can and can't do, with tests pinning those properties down: otherwise someone could later casually
// add a method to the template context, or a readFile to the FuncMap, and the capability surface would silently
// grow while the diff looks like just a harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can both read a field and call a method). So as soon as the template
// context can reach **any** type with exported methods, those methods are exposed to the template author.
// This feature's context is deliberately pure data (exported fields only, zero methods).
//
// If this fails: someone added a method to webhookTemplateData / webhookItem.
// Before deciding to allow it, think hard about whether the template could use that method to read something that
// shouldn't be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, "+
				"which opens up those methods' capabilities to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks in the set of functions exposed to templates.
//
// Every extra function in the FuncMap is an extra capability. Currently there are only json / jsons, which serialise
// values into JSON fragments: they can't read files, send requests or run commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the set of template functions changed: got %v, expected %v. Before adding a function, confirm it doesn't widen the capability surface"+
			" (no reading or writing files, no network requests, no running commands)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in templates:
// accessing something that doesn't exist must fail rather than echo something back, and the failure message must not
// carry out internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a field that doesn't exist should return an error")
	}
	// The error must not contain real content from the template context (the vulnerability title/summary).
	for _, leak := range []string{"SQL injection", "Parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the template error leaks message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a wrong template is a config error that retrying won't fix.
// If it were judged retryable, one bad template would make every delivery waste three rounds of backoff.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("a template syntax error should be caught when saving")
	}
	// Even if validation is bypassed and it is delivered directly, it must be judged a permanent failure rather than retried over and over.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be judged a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that "the template render result must be valid JSON".
// It also blocks uses such as "generating plain text with the template to trigger some other protocol".
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// Rendering non-JSON must be refused (rather than sent out as is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("rendering non-JSON should be judged a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("the error message should say it is a JSON problem, got %v", err)
	}
}
