package wire

import "testing"

type inner struct {
	A string `json:"a"`
}

type outer struct {
	N  int64   `json:"n"`
	S  string  `json:"s"`
	In []inner `json:"in"`
}

func TestStrictDecode(t *testing.T) {
	good := `{"n":5,"s":"x","in":[{"a":"b"}]}`
	var o outer
	if err := StrictDecode([]byte(good), &o); err != nil || o.N != 5 || o.In[0].A != "b" {
		t.Fatalf("good: %v %+v", err, o)
	}
	bad := map[string]string{
		"duplicate top":    `{"n":5,"n":6,"s":"x","in":[]}`,
		"duplicate nested": `{"n":5,"s":"x","in":[{"a":"b","a":"c"}]}`,
		"unknown key":      `{"n":5,"s":"x","in":[],"z":1}`,
		"unknown nested":   `{"n":5,"s":"x","in":[{"a":"b","z":"c"}]}`,
		"case alias":       `{"n":5,"N":6,"s":"x","in":[]}`,
		"nested alias":     `{"n":5,"s":"x","in":[{"a":"b","A":"c"}]}`,
		"missing key":      `{"n":5,"in":[]}`,
		"missing nested":   `{"n":5,"s":"x","in":[{}]}`,
		"float":            `{"n":5.0,"s":"x","in":[]}`,
		"exponent":         `{"n":5e1,"s":"x","in":[]}`,
		"negative":         `{"n":-5,"s":"x","in":[]}`,
		"leading zero":     `{"n":05,"s":"x","in":[]}`,
		"too big":          `{"n":9007199254740992,"s":"x","in":[]}`,
		"null":             `{"n":null,"s":"x","in":[]}`,
		"wrong type":       `{"n":"5","s":"x","in":[]}`,
		"trailing":         `{"n":5,"s":"x","in":[]} {}`,
		"trailing junk":    `{"n":5,"s":"x","in":[]}x`,
		"array top":        `[1]`,
		"bad utf8":         "{\"n\":5,\"s\":\"\xff\",\"in\":[]}",
	}
	for name, in := range bad {
		var o outer
		if err := StrictDecode([]byte(in), &o); err == nil {
			t.Errorf("%s: accepted %s", name, in)
		}
	}
	var o2 outer
	if err := StrictDecode([]byte(`{"n":9007199254740991,"s":"x","in":[]}`), &o2); err != nil {
		t.Errorf("max int rejected: %v", err)
	}
}

func TestEscapes(t *testing.T) {
	var o outer
	if err := StrictDecode([]byte(`{"n":1,"s":"a\/b\"c\\d","in":[]}`), &o); err != nil || o.S != `a/b"c\d` {
		t.Fatalf("allowed escapes: %v %q", err, o.S)
	}
	for _, in := range []string{`{"n":1,"s":"\u0041","in":[]}`, `{"n":1,"s":"\n","in":[]}`, `{"n":1,"s":"\ud800","in":[]}`, "{\"n\":1,\"s\":\"a\tb\",\"in\":[]}"} {
		if err := StrictDecode([]byte(in), &o); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestB64(t *testing.T) {
	if _, err := DecodeB64Exact("AAE", 2); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"AAE=", "AA+", "AAF"} {
		if _, err := DecodeB64(s, 0, 10); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
