package guard

import "testing"

func TestLastNumber(t *testing.T) {
	k, why := lastNumber("The tool result confirms it.\nSo it is good.\n\n1", 2)
	if k != 0 || why != "The tool result confirms it. So it is good." {
		t.Fatalf("%d %q", k, why)
	}
	if k, _ := lastNumber("It contradicts the timeout.\n2", 2); k != 1 {
		t.Fatalf("got %d", k)
	}
	if k, why := lastNumber("no number here", 2); k != 0 || why != "" {
		t.Fatalf("%d %q", k, why)
	}
	if k := pickNumber("2", 2); k != 1 {
		t.Fatal(k)
	}
}
