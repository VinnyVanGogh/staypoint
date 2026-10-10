package workorgs

import (
	"reflect"
	"testing"
)

func TestDefaultAlwaysWork(t *testing.T) {
	t.Cleanup(func() { Set(nil) })
	Set(nil)
	if !IsWork("Managed Solution") || !IsWork(" managed solution ") || !IsWork("MAN") {
		t.Fatal("Managed Solution must always be work")
	}
	if IsWork("Power Platform") || IsWork("StayPoint") || IsWork("") {
		t.Fatal("unconfigured orgs must not be work")
	}
}

func TestSetAddsOrgsAndKeepsDefault(t *testing.T) {
	t.Cleanup(func() { Set(nil) })
	Set([]string{"power platform", " Power Platform ", "", "managed solution"})
	if got, want := List(), []string{"Managed Solution", "power platform"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	if !IsWork("Power Platform") || !IsWork("Managed Solution") {
		t.Fatal("configured and default orgs must be work")
	}
	if IsWork("StayPoint") || IsWork("Pоwer Platform") { // Cyrillic о
		t.Fatal("other orgs and lookalikes must not be work")
	}
}
