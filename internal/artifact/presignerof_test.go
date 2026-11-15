package artifact

import (
	"net/http/httptest"
	"testing"

	"toft/internal/telemetry/telemetryharness"
)

// The telemetry decorator embeds only Store, so a bare type assertion on a
// wrapped S3 store finds no presigner. PresignerOf has to see through it, or
// no isolated workload can start on an S3 install with telemetry on.
func TestPresignerOfSeesThroughTheTelemetryDecorator(t *testing.T) {
	telemetryharness.Enable(t)
	srv := httptest.NewServer(newFakeS3())
	defer srv.Close()

	wrapped := Instrumented(newTestS3Store(t, srv, ""), "s3")
	if _, ok := wrapped.(instrumented); !ok {
		t.Fatal("telemetry is on, so the store should be wrapped")
	}
	if _, ok := wrapped.(Presigner); ok {
		t.Fatal("the decorator now forwards Presigner itself; this test no longer pins the trap")
	}
	p, ok := PresignerOf(wrapped)
	if !ok {
		t.Fatal("PresignerOf did not find the S3 store's presigner under the decorator")
	}
	if _, err := p.PresignGet(t.Context(), "releases/a/dep.tar.gz", PresignTTL); err != nil {
		t.Fatalf("presign through PresignerOf: %v", err)
	}
}

// A store that cannot presign is not made to claim it can by the wrapper.
func TestPresignerOfReportsAStoreThatCannotPresign(t *testing.T) {
	telemetryharness.Enable(t)
	if _, ok := PresignerOf(nil); ok {
		t.Error("a nil store has no presigner")
	}
	if _, ok := PresignerOf(Instrumented(fakeStoreOnly{}, "test")); ok {
		t.Error("a wrapped store with no presigner reported one")
	}
}

type fakeStoreOnly struct{ Store }

// TL-9: the decorator is installed whether or not telemetry is on at boot,
// because it can be switched on from Admin → Telemetry afterwards.
func TestInstrumentedWrapsWithTelemetryOff(t *testing.T) {
	if _, ok := Instrumented(fakeStoreOnly{}, "test").(instrumented); !ok {
		t.Error("with telemetry off the store was returned bare; enabling it later would record nothing")
	}
	if Instrumented(nil, "test") != nil {
		t.Error("a nil store must stay nil")
	}
}
