package jsonobj

import (
	"strings"
	"testing"
)

func TestParseKeepsOrderAndNestedValues(t *testing.T) {
	in := `{"zeta": 1, "alpha": {"b": [1, 2], "a": "x"}, "mid": null}`
	o, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(o.Keys(), ","); got != "zeta,alpha,mid" {
		t.Fatalf("keys = %s", got)
	}
	if err := o.SetValue("new", "<v&>"); err != nil {
		t.Fatal(err)
	}
	out, err := o.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "zeta": 1,
  "alpha": {
    "b": [
      1,
      2
    ],
    "a": "x"
  },
  "mid": null,
  "new": "<v&>"
}
`
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestEmptyInputIsEmptyObject(t *testing.T) {
	o, err := Parse([]byte("  \n"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Len() != 0 {
		t.Fatalf("len = %d", o.Len())
	}
}

func TestRejectsNonObjectAndTrailingData(t *testing.T) {
	for _, in := range []string{`[1]`, `"x"`, `{"a":1} {"b":2}`, `{"a":1`, `{"a": 1} // comment`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
		}
	}
}

func TestSetKeepsPositionAndDelete(t *testing.T) {
	o, _ := Parse([]byte(`{"a":1,"b":2,"c":3}`))
	_ = o.SetValue("b", 20)
	o.Delete("a")
	o.Delete("missing")
	if got := strings.Join(o.Keys(), ","); got != "b,c" {
		t.Fatalf("keys = %s", got)
	}
	if v, _ := o.Get("b"); string(v) != "20" {
		t.Fatalf("b = %s", v)
	}
}

func TestGetObject(t *testing.T) {
	o, _ := Parse([]byte(`{"env":{"X":"1"},"n":3}`))
	env, err := o.GetObject("env")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := env.GetString("X"); !ok || s != "1" {
		t.Fatalf("X = %q %v", s, ok)
	}
	if _, err := o.GetObject("n"); err == nil {
		t.Fatal("GetObject on a number succeeded")
	}
	missing, err := o.GetObject("none")
	if err != nil || missing.Len() != 0 {
		t.Fatalf("missing = %v, %v", missing, err)
	}
}
