package calldiff

import (
	"testing"
)

func paths(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, string(c.Kind)+" "+c.Path)
	}
	return out
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The whole reason this is structural: key order and whitespace are not
// changes, and a textual diff would report both and bury the real one.
func TestReorderedKeysAreNotChanges(t *testing.T) {
	a := []byte(`{"id":"ORD-1","customer":"Andes","total":{"currency":"CLP","amountCents":"100"}}`)
	b := []byte(`{
	  "total": { "amountCents": "100", "currency": "CLP" },
	  "customer": "Andes",
	  "id": "ORD-1"
	}`)

	changes, err := StructuralJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no differences, got %v", paths(changes))
	}
}

func TestFindsTheOneFieldThatDiffers(t *testing.T) {
	a := []byte(`{"id":"ORD-1","status":"STATUS_PENDING","lines":[{"sku":"ABC-9","quantity":3}]}`)
	b := []byte(`{"id":"ORD-1","status":"STATUS_SHIPPED","lines":[{"sku":"ABC-9","quantity":3}]}`)

	changes, err := StructuralJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, paths(changes), []string{"changed status"})
	if changes[0].A != "STATUS_PENDING" || changes[0].B != "STATUS_SHIPPED" {
		t.Errorf("change carries %v -> %v", changes[0].A, changes[0].B)
	}
}

func TestAddedAndRemovedFields(t *testing.T) {
	a := []byte(`{"id":"ORD-1","note":"urgente"}`)
	b := []byte(`{"id":"ORD-1","channel":"web"}`)

	changes, err := StructuralJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, paths(changes), []string{"added channel", "removed note"})
}

// Position carries meaning in a protobuf repeated field, so array order is a
// difference even though object key order is not.
func TestArrayOrderMatters(t *testing.T) {
	a := []byte(`{"lines":[{"sku":"ABC-9"},{"sku":"XYZ-1"}]}`)
	b := []byte(`{"lines":[{"sku":"XYZ-1"},{"sku":"ABC-9"}]}`)

	changes, err := StructuralJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, paths(changes), []string{"changed lines[0].sku", "changed lines[1].sku"})
}

func TestLengthDifferenceInArrays(t *testing.T) {
	a := []byte(`{"lines":[{"sku":"ABC-9"}]}`)
	b := []byte(`{"lines":[{"sku":"ABC-9"},{"sku":"XYZ-1"}]}`)

	changes, err := StructuralJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, paths(changes), []string{"added lines[1]"})
}

func TestNestedPaths(t *testing.T) {
	a := []byte(`{"lines":[{"price":{"currency":"CLP","amountCents":"1290000"}}]}`)
	b := []byte(`{"lines":[{"price":{"currency":"USD","amountCents":"1290000"}}]}`)

	changes, err := StructuralJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, paths(changes), []string{"changed lines[0].price.currency"})
}

// protojson renders int64 as a string while a hand-written JSON body may carry
// a bare number. Reporting that as a difference would be noise about encoding,
// not about the value.
func TestNumberAndStringOfTheSameValueMatch(t *testing.T) {
	a := []byte(`{"amountCents":"1290000"}`)
	b := []byte(`{"amountCents":1290000}`)

	changes, err := StructuralJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no differences, got %v", paths(changes))
	}
}

func TestNullIsDistinctFromAbsentAndFromEmpty(t *testing.T) {
	changes, err := StructuralJSON([]byte(`{"note":null}`), []byte(`{"note":""}`))
	if err != nil {
		t.Fatal(err)
	}
	equal(t, paths(changes), []string{"changed note"})
}

func TestRootTypeChange(t *testing.T) {
	changes, err := StructuralJSON([]byte(`{"a":1}`), []byte(`[1]`))
	if err != nil {
		t.Fatal(err)
	}
	equal(t, paths(changes), []string{"changed (root)"})
}

func TestNonJSONIsReportedNotGuessed(t *testing.T) {
	if _, err := StructuralJSON([]byte("plain text"), []byte(`{"a":1}`)); err == nil {
		t.Error("expected an error so the caller can fall back to comparing bytes")
	}
}
