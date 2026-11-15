package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// API-APPS-2: "max 100MB" was ParseMultipartForm's memory threshold, which
// refuses nothing. The body is now limited before the form is read.
func TestAnUploadBodyOverTheCapIsRefused(t *testing.T) {
	const boundary = "----tofttest"
	head := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"big.zip\"\r\n\r\n"
	tail := "\r\n--" + boundary + "--\r\n"
	body := io.MultiReader(strings.NewReader(head),
		io.LimitReader(zeros{}, maxUploadBytes+uploadFormOverhead+1), strings.NewReader(tail))

	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	limitUploadBody(rec, req)
	if err := req.ParseMultipartForm(1 << 20); err == nil {
		t.Fatal("a body past the cap parsed")
	}
	if req.MultipartForm != nil {
		req.MultipartForm.RemoveAll()
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// API-APPS-4: two uploads to one app extracted into its one staging
// directory. The slot is held per app; the second claim is refused until the
// first is released, and another app is unaffected.
func TestAnAppsUploadSlotIsHeldOnce(t *testing.T) {
	release, ok := claimUploadSlot("app-slot")
	if !ok {
		t.Fatal("first claim refused")
	}
	if _, ok := claimUploadSlot("app-slot"); ok {
		t.Fatal("a second upload to the same app took the slot")
	}
	other, ok := claimUploadSlot("app-other")
	if !ok {
		t.Fatal("another app's upload was refused")
	}
	other()
	release()
	again, ok := claimUploadSlot("app-slot")
	if !ok {
		t.Fatal("the slot was not released")
	}
	again()
}
