package api

import (
	"reflect"
	"testing"

	"toft/internal/artifact"
)

// The signer is an interface, and an interface field has two ways to be empty.
//
// `storeSigner` used to be a *artifact.BucketSigner, where nil meant absent and
// the compiler made that the only way to say it. Now it is artifact.Signer, and
// a nil *S3Signer assigned into it is a non-nil interface holding a nil pointer
// — storeUploadsOffered would say the routes are available and every one of
// them would panic on the first call.
//
// So this asserts both halves: the plain absence still 404s, and the option
// that sets the field is typed as the interface, which is what keeps a caller
// from constructing the trap by declaring their variable concretely.
func TestStoreSignerAbsenceIsTheInterfaceNil(t *testing.T) {
	s := &Server{}
	if s.storeUploadsOffered() {
		t.Fatal("store uploads offered with no storage and no signer")
	}

	// WithStoreSigner must take the interface. If it takes a concrete type the
	// caller's variable is concrete too, and main.go's nil is the wrong nil.
	param := reflect.TypeOf(WithStoreSigner).In(0)
	if param.Kind() != reflect.Interface {
		t.Fatalf("WithStoreSigner takes %v; it must take the artifact.Signer interface so "+
			"an absent signer is a nil interface rather than a typed nil pointer", param)
	}
	if want := reflect.TypeOf((*artifact.Signer)(nil)).Elem(); param != want {
		t.Fatalf("WithStoreSigner takes %v, want %v", param, want)
	}
}
