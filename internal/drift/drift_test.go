package drift

import (
	"strings"
	"testing"
)

func shape(t *testing.T, payload string) Shape {
	t.Helper()
	s, err := Of([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func compare(t *testing.T, before, after string) []Change {
	t.Helper()
	return Compare(shape(t, before), shape(t, after))
}

func find(changes []Change, path string) *Change {
	for i := range changes {
		if changes[i].Path == path {
			return &changes[i]
		}
	}
	return nil
}

// Values change every call. Shapes are not supposed to, and telling the two
// apart is the whole reason this exists.
func TestDifferentValuesAreNotDrift(t *testing.T) {
	changes := compare(t,
		`{"total":1290,"currency":"CLP","items":[{"sku":"A"}]}`,
		`{"total":4560,"currency":"USD","items":[{"sku":"B"},{"sku":"C"}]}`)

	if len(changes) != 0 {
		t.Errorf("different values were reported as drift: %v", changes)
	}
}

// The three that matter, and the one that breaks a caller hardest.
func TestAFieldThatWentAway(t *testing.T) {
	changes := compare(t, `{"total":1,"currency":"CLP"}`, `{"total":1}`)

	c := find(changes, "currency")
	if c == nil || c.Kind != Gone {
		t.Fatalf("the missing field was not reported: %v", changes)
	}
	if c.Was != "string" {
		t.Errorf("it does not say what the field used to be: %+v", c)
	}
}

func TestAFieldThatChangedType(t *testing.T) {
	changes := compare(t, `{"total":1290000}`, `{"total":"1290000"}`)

	c := find(changes, "total")
	if c == nil || c.Kind != Retyped {
		t.Fatalf("the type change was not reported: %v", changes)
	}
	if c.Was != "number" || c.Now != "string" {
		t.Errorf("the change reads as %s -> %s", c.Was, c.Now)
	}
}

func TestAFieldThatAppeared(t *testing.T) {
	changes := compare(t, `{"total":1}`, `{"total":1,"cached":true}`)

	c := find(changes, "cached")
	if c == nil || c.Kind != Added {
		t.Fatalf("the new field was not reported: %v", changes)
	}
	if c.Now != "boolean" {
		t.Errorf("it does not say what the new field is: %+v", c)
	}
}

// A list of two hundred orders has the shape of one order. Reporting it two
// hundred times would bury the field that actually changed.
func TestAListCollapsesToTheShapeOfItsItems(t *testing.T) {
	s := shape(t, `{"items":[{"sku":"A","qty":1},{"sku":"B","qty":2},{"sku":"C","qty":3}]}`)

	if got := len(s); got != 2 {
		t.Errorf("%d entries for a three-item list, want 2: %v", got, s)
	}
	if s["items[].sku"] != "string" || s["items[].qty"] != "number" {
		t.Errorf("the item shape is wrong: %v", s)
	}
}

// Drift inside a list is exactly the case a flat comparison misses.
func TestDriftInsideAList(t *testing.T) {
	changes := compare(t,
		`{"items":[{"sku":"A","currency":"CLP"}]}`,
		`{"items":[{"sku":"A"}]}`)

	if c := find(changes, "items[].currency"); c == nil || c.Kind != Gone {
		t.Fatalf("a field lost inside a list was not reported: %v", changes)
	}
}

// A nullable field is ordinary. Flagging every one of them buries the changes
// that matter under noise nobody can act on.
func TestANullableFieldIsNotDrift(t *testing.T) {
	if changes := compare(t, `{"note":null}`, `{"note":"algo"}`); len(changes) != 0 {
		t.Errorf("null becoming a string was reported as drift: %v", changes)
	}
	if changes := compare(t, `{"note":"algo"}`, `{"note":null}`); len(changes) != 0 {
		t.Errorf("a string becoming null was reported as drift: %v", changes)
	}
	// But the field going away entirely still is.
	if changes := compare(t, `{"note":null}`, `{}`); len(changes) != 1 {
		t.Errorf("a nullable field disappearing was not reported: %v", changes)
	}
}

// An empty list says nothing about what it holds, and guessing would invent a
// contract nobody wrote.
func TestAnEmptyListClaimsNothing(t *testing.T) {
	s := shape(t, `{"items":[]}`)
	if s["items[]"] != "empty list" {
		t.Errorf("an empty list was read as %v", s)
	}
	// And filling it later is an addition, not a type change.
	changes := compare(t, `{"items":[]}`, `{"items":[{"sku":"A"}]}`)
	for _, c := range changes {
		if c.Kind == Retyped {
			t.Errorf("filling an empty list was read as a type change: %+v", c)
		}
	}
}

// Adding a field is safe; losing one or changing its type is what takes a
// client down. A report that does not separate them is a list worth ignoring.
func TestOnlyLossesAndTypeChangesBreakACaller(t *testing.T) {
	changes := compare(t,
		`{"total":1,"currency":"CLP"}`,
		`{"total":"1","cached":true}`)

	breaking := Breaking(changes)
	if len(breaking) != 2 {
		t.Fatalf("%d breaking changes, want the loss and the retype: %v", len(breaking), breaking)
	}
	for _, c := range breaking {
		if c.Kind == Added {
			t.Errorf("an added field was called breaking: %+v", c)
		}
	}
}

func TestSomethingThatIsNotJSONSaysSo(t *testing.T) {
	_, err := Of([]byte("<xml>no</xml>"))
	if err == nil {
		t.Fatal("XML was accepted as a shape")
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("the error does not say why: %v", err)
	}
}

func TestRenderReadsLikeADiff(t *testing.T) {
	out := Render(compare(t,
		`{"total":1,"currency":"CLP"}`,
		`{"total":"1","cached":true}`))

	for _, want := range []string{"- currency", "~ total", "number -> string", "+ cached"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, out)
		}
	}
	if got := Render(nil); !strings.Contains(got, "not changed") {
		t.Errorf("no drift renders as %q", got)
	}
}
