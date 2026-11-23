package notification

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		vars        map[string]string
		want        string
		wantMissing []string
	}{
		{"simple", "Hi {{name}}!", map[string]string{"name": "Ayse"}, "Hi Ayse!", nil},
		{"spaces inside braces", "Hi {{ name }}!", map[string]string{"name": "Ali"}, "Hi Ali!", nil},
		{"repeated", "{{x}}-{{x}}", map[string]string{"x": "1"}, "1-1", nil},
		{"extra vars ignored", "Hi {{name}}", map[string]string{"name": "A", "other": "B"}, "Hi A", nil},
		{"no placeholders", "Plain text", nil, "Plain text", nil},
		{"values are not re-parsed", "Hi {{name}}", map[string]string{"name": "{{evil}}"}, "Hi {{evil}}", nil},
		{"missing reported sorted", "{{b}} {{a}} {{b}}", map[string]string{}, "", []string{"a", "b"}},
		{"empty value allowed", "[{{x}}]", map[string]string{"x": ""}, "[]", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, missing := Render(tt.content, tt.vars)
			if got != tt.want || !reflect.DeepEqual(missing, tt.wantMissing) {
				t.Errorf("Render = %q, missing %v; want %q, %v", got, missing, tt.want, tt.wantMissing)
			}
		})
	}
}

func TestPlaceholders(t *testing.T) {
	got := Placeholders("{{b}} {{ a }} {{b}} {{c_1}}")
	if want := []string{"b", "a", "c_1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestValidateTemplate(t *testing.T) {
	ok := TemplateInput{Channel: ChannelSMS, Content: "Hi {{name}}"}
	tests := []struct {
		name      string
		tplName   string
		in        TemplateInput
		wantField string
	}{
		{"valid", "order_shipped", ok, ""},
		{"uppercase name", "Order", ok, "name"},
		{"name with space", "a b", ok, "name"},
		{"bad channel", "x", TemplateInput{Channel: "fax", Content: "hi"}, "channel"},
		{"empty content", "x", TemplateInput{Channel: ChannelSMS, Content: " "}, "content"},
		{"space in variable name", "x", TemplateInput{Channel: ChannelSMS, Content: "Hi {{first name}}"}, "content"},
		{"unclosed placeholder", "x", TemplateInput{Channel: ChannelSMS, Content: "Hi {{name}"}, "content"},
		{"too long for sms", "x", TemplateInput{Channel: ChannelSMS, Content: strings.Repeat("a", MaxSMSLength+1)}, "content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateTemplate(tt.tplName, tt.in)
			if tt.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("expected valid, got %+v", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Field != tt.wantField {
				t.Fatalf("expected one error on %q, got %+v", tt.wantField, errs)
			}
		})
	}
}

func TestApplyTemplate(t *testing.T) {
	tpl := Template{Name: "shipped", Channel: ChannelSMS, Content: "Hi {{name}}, order {{order}} shipped"}

	in := CreateInput{Channel: ChannelSMS, Recipient: "+905551234567", Priority: PriorityNormal, Template: "shipped", Variables: map[string]string{"name": "Ayse", "order": "42"}}
	if errs := in.ApplyTemplate(tpl, ""); errs != nil {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if in.Content != "Hi Ayse, order 42 shipped" {
		t.Errorf("content = %q", in.Content)
	}
	if errs := in.Validate("", time.Now()); len(errs) != 0 {
		t.Errorf("rendered input should validate: %+v", errs)
	}

	cases := map[string]CreateInput{
		"variables":          {Channel: ChannelSMS, Template: "shipped", Variables: map[string]string{"name": "A"}},
		"template":           {Channel: ChannelEmail, Template: "shipped"},
		"content":            {Channel: ChannelSMS, Template: "shipped", Content: "also content"},
		"items[3].variables": {Channel: ChannelSMS, Template: "shipped"},
	}
	for wantField, in := range cases {
		prefix := ""
		if strings.HasPrefix(wantField, "items[3].") {
			prefix = "items[3]."
		}
		errs := in.ApplyTemplate(tpl, prefix)
		if len(errs) != 1 || errs[0].Field != wantField {
			t.Errorf("want error on %q, got %+v", wantField, errs)
		}
	}
}

func TestVariablesWithoutTemplate(t *testing.T) {
	in := CreateInput{Channel: ChannelSMS, Recipient: "+905551234567", Content: "hi", Priority: PriorityNormal, Variables: map[string]string{"a": "b"}}
	errs := in.Validate("", time.Now())
	if len(errs) != 1 || errs[0].Field != "variables" {
		t.Errorf("got %+v", errs)
	}
}

func TestTemplateStore(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	tpl, created, err := store.PutTemplate(ctx, "welcome", TemplateInput{Channel: ChannelEmail, Content: "Welcome, {{name}}!"})
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if !reflect.DeepEqual(tpl.Variables, []string{"name"}) {
		t.Errorf("variables = %v", tpl.Variables)
	}

	_, created, err = store.PutTemplate(ctx, "welcome", TemplateInput{Channel: ChannelEmail, Content: "Hello again, {{name}}"})
	if err != nil || created {
		t.Fatalf("replace: created=%v err=%v", created, err)
	}
	got, err := store.GetTemplate(ctx, "welcome")
	if err != nil || got.Content != "Hello again, {{name}}" {
		t.Fatalf("get after replace: %+v %v", got, err)
	}

	if _, _, err := store.PutTemplate(ctx, "alert", TemplateInput{Channel: ChannelPush, Content: "Alert"}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListTemplates(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "alert" {
		t.Fatalf("list: %+v %v", list, err)
	}

	// A notification created from a template keeps the template's name and
	// the rendered text, even after the template is deleted.
	in := CreateInput{Recipient: "a@example.com", Channel: ChannelEmail, Priority: PriorityNormal, Template: "welcome", Variables: map[string]string{"name": "Ayse"}}
	if errs := in.ApplyTemplate(got, ""); errs != nil {
		t.Fatal(errs)
	}
	n := New(in, nil, "", 3, time.Now())
	if _, err := store.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTemplate(ctx, "welcome"); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TemplateName == nil || *saved.TemplateName != "welcome" || saved.Content != "Hello again, Ayse" {
		t.Errorf("saved notification: template=%v content=%q", saved.TemplateName, saved.Content)
	}

	if _, err := store.GetTemplate(ctx, "welcome"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("get deleted: %v", err)
	}
	if err := store.DeleteTemplate(ctx, "welcome"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}
