package api

import (
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/projectconfig"
)

// Switching image transformation on used to save, toast "Successfully updated storage settings" and leave every
// render URL answering 404: the node runs no imgproxy. It is refused with the reason, on both routes.
func TestImageTransformationIsRefusedNotSilentlyAccepted(t *testing.T) {
	f := newFixture(t)
	on := map[string]any{"features": map[string]any{"imageTransformation": map[string]any{"enabled": true}}}
	for _, path := range []string{"/platform/projects/" + testRef + "/config/storage", "/v1/projects/" + testRef + "/config/storage"} {
		rec := f.do("PATCH", path, on)
		if rec.Code != 400 {
			t.Fatalf("PATCH %s: %d %s", path, rec.Code, rec.Body)
		}
		msg := bodyMap(t, rec)["message"].(string)
		for _, want := range []string{"imgproxy", "[storage.image_transformation] enabled = false"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q lacks %q", msg, want)
			}
		}
	}
	// Nothing was saved with it, so the answer to a reload says off.
	st := "/platform/projects/" + testRef + "/config/storage"
	feats := bodyMap(t, f.mustDo("GET", st, nil, 200))["features"].(map[string]any)
	if feats["imageTransformation"].(map[string]any)["enabled"] != false {
		t.Errorf("after a refused save the setting says %v", feats["imageTransformation"])
	}
	// The same save with other changes in it is refused as a whole.
	f.mustDo("PATCH", st, map[string]any{"fileSizeLimit": 1048576, "features": on["features"]}, 400)
	if v := bodyMap(t, f.mustDo("GET", st, nil, 200))["fileSizeLimit"]; v == float64(1048576) {
		t.Error("the refused save changed the upload limit")
	}
	// Off passes, with the whole features object Studio sends with every change of the limit.
	f.mustDo("PATCH", st, map[string]any{"fileSizeLimit": 1048576, "features": map[string]any{
		"imageTransformation": map[string]any{"enabled": false}, "s3Protocol": map[string]any{"enabled": true}}}, 200)
	// The CLI's push of its default config.toml names the setting it would change.
	f.mustDo("PATCH", "/v1/projects/"+testRef+"/config/storage", map[string]any{"fileSizeLimit": 1048576, "features": map[string]any{
		"imageTransformation": map[string]any{"enabled": false}, "s3Protocol": map[string]any{"enabled": true}}}, 200)
}

// A project that saved the setting on, before the refusal existed, keeps saving the rest of its storage settings:
// the value is not a change.
func TestImageTransformationAlreadySavedOnIsNotRefusedAgain(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.settings.Patch(t.Context(), testRef, projectconfig.Storage, map[string]any{
		"features": map[string]any{"imageTransformation": map[string]any{"enabled": true}}}, projectconfig.CrossContext{}); err != nil {
		t.Fatal(err)
	}
	st := "/platform/projects/" + testRef + "/config/storage"
	got := bodyMap(t, f.mustDo("PATCH", st, map[string]any{"fileSizeLimit": 2097152, "features": map[string]any{
		"imageTransformation": map[string]any{"enabled": true}}}, 200))
	if got["fileSizeLimit"] != float64(2097152) {
		t.Errorf("fileSizeLimit %v", got["fileSizeLimit"])
	}
	// Switching it off works, and then on is refused again.
	f.mustDo("PATCH", st, map[string]any{"features": map[string]any{"imageTransformation": map[string]any{"enabled": false}}}, 200)
	f.mustDo("PATCH", st, map[string]any{"features": map[string]any{"imageTransformation": map[string]any{"enabled": true}}}, 400)
}
