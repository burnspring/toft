package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// maxUploadBytes is the documented cap on an uploaded archive. It was only
// ever ParseMultipartForm's argument, which is a memory threshold and refuses
// nothing — a multi-gigabyte body spooled to TMPDIR and was then walked
// (API-APPS-2). limitUploadBody makes it a cap.
const maxUploadBytes int64 = 100 << 20

// uploadFormOverhead is room for the multipart framing and the form's other
// fields beside the file.
const uploadFormOverhead int64 = 1 << 20

// limitUploadBody bounds the request body at the cap before anything reads
// it, so ParseMultipartForm fails on a body over it instead of storing it.
func limitUploadBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+uploadFormOverhead)
}

// maxArchiveStreamBytes bounds how much a tar.gz may decompress to on a
// single pass. A tar has no directory, so the planning pass decompresses the
// whole stream before maxUnzippedBytes — counted over what is written — gets a
// say: a hundred megabytes of gzipped zeros is a hundred gigabytes of CPU with
// nothing written. Far above maxUnzippedBytes, because the excluded freight
// (`node_modules`, `.git`) is in the stream and costs no written byte. A
// variable for the test that lowers it.
var maxArchiveStreamBytes int64 = 4 << 30

// errArchiveStreamTooLarge is the refusal capReader returns.
var errArchiveStreamTooLarge = errors.New("archive expands past the limit")

// capReader reads at most max bytes from r and fails after that.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("%w: it decompresses to more than %d MB", errArchiveStreamTooLarge, maxArchiveStreamBytes>>20)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// uploadsInFlight holds the apps an upload is being staged for. The staging
// directory is one per app, and HasPendingDeployment is read before the body
// — the window is the whole upload — so two uploads to one app at once both
// passed it and extracted into the same directory, and the build that ran
// served a tree that was neither archive (API-APPS-4). The claim spans the
// check through the hand-off; the queued row answers every upload after it.
// Per process: two platform nodes can still cross, which the pending-row check
// narrows to the length of one extract.
var uploadsInFlight sync.Map

// claimUploadSlot takes the app's staging slot, or reports that another
// upload holds it. The returned release must be called.
func claimUploadSlot(appID string) (release func(), ok bool) {
	if _, held := uploadsInFlight.LoadOrStore(appID, struct{}{}); held {
		return func() {}, false
	}
	return func() { uploadsInFlight.Delete(appID) }, true
}

// connectBundlesInFlight is uploadsInFlight for the Connect bundle upload:
// the count of waiting bundles is read before the body arrives, so a claim
// spans the count through the write (AP-6). Per process, as uploadsInFlight.
var connectBundlesInFlight sync.Map

// maxWaitingConnectBundles is how many uploaded-but-undeployed bundles one
// content item may hold. rsconnect uploads one and deploys it at once.
const maxWaitingConnectBundles = 3

// claimConnectBundleSlot takes the app's bundle-upload slot, or reports that
// another upload holds it. The returned release must be called.
func claimConnectBundleSlot(appID string) (release func(), ok bool) {
	if _, held := connectBundlesInFlight.LoadOrStore(appID, struct{}{}); held {
		return func() {}, false
	}
	return func() { connectBundlesInFlight.Delete(appID) }, true
}

// refuseConcurrentUpload is the 409 for an upload that lost the slot.
func refuseConcurrentUpload(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": "another upload to this app is in progress; wait for it and deploy again",
		"code":  "deploy_queued"})
}
