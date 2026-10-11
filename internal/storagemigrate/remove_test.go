package storagemigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

func removerOver(b Bucket, mut func(*config.Config)) (*ObjectRemover, *[]Credentials) {
	cfg := config.Default()
	cfg.Fleet.StorageBackend, cfg.Fleet.StorageS3Bucket, cfg.Fleet.StorageS3Region = "s3", "objects", "eu-west-1"
	cfg.Fleet.StorageS3AccessKeyID, cfg.Fleet.StorageS3SecretAccessKey = "AKIA", "shh"
	if mut != nil {
		mut(cfg)
	}
	var seen []Credentials
	return &ObjectRemover{Cfg: cfg, Open: func(_ context.Context, d Destination, c Credentials) (Bucket, error) {
		seen = append(seen, c)
		return b, nil
	}}, &seen
}

// Only the keys under <ref>/ go, in batches, and another project's prefix, which starts with the same
// letters in no case but shares the bucket, stays.
func TestRemoveObjectsEmptiesOnePrefix(t *testing.T) {
	b := newMemBucket()
	for i := range 1200 {
		b.objs[fmt.Sprintf("%s/avatars/f%04d/v1", refA, i)] = memObj{data: []byte("x")}
	}
	b.objs[refB+"/avatars/keep/v1"] = memObj{data: []byte("x")}
	b.objs[refA+"x/looks-similar"] = memObj{data: []byte("x")} // not under "<refA>/"
	r, creds := removerOver(b, nil)
	if err := r.RemoveObjects(context.Background(), refA); err != nil {
		t.Fatal(err)
	}
	for k := range b.objs {
		if strings.HasPrefix(k, refA+"/") {
			t.Fatalf("%s is still there", k)
		}
	}
	if len(b.objs) != 2 || len(b.dels) != 1200 {
		t.Fatalf("objects left = %d, deletes = %d", len(b.objs), len(b.dels))
	}
	if len(*creds) != 1 || (*creds)[0].Source != CredConfig || (*creds)[0].AccessKeyID != "AKIA" {
		t.Fatalf("credentials = %+v", *creds)
	}
}

func TestRemoveObjectsRefusals(t *testing.T) {
	ctx := context.Background()
	b := newMemBucket()
	b.objs[refA+"/o"] = memObj{data: []byte("x")}

	r, _ := removerOver(b, func(c *config.Config) { c.Fleet.StorageBackend = "" })
	if err := r.RemoveObjects(ctx, refA); err != nil || len(b.objs) != 1 {
		t.Fatalf("file backend: %v, %d objects", err, len(b.objs))
	}
	r, _ = removerOver(b, nil)
	for _, bad := range []string{"", "..", "a/b", "short"} {
		if err := r.RemoveObjects(ctx, bad); err == nil {
			t.Errorf("RemoveObjects(%q) was accepted", bad)
		}
	}
	if len(b.objs) != 1 {
		t.Fatal("a refused ref removed objects")
	}
	// A role is enough; neither a key nor a role is an explained error.
	r, creds := removerOver(b, func(c *config.Config) {
		c.Fleet.StorageS3AccessKeyID, c.Fleet.StorageS3SecretAccessKey, c.Fleet.StorageS3RoleARN = "", "", "arn:aws:iam::1:role/s"
	})
	if err := r.RemoveObjects(ctx, refA); err != nil || (*creds)[0].RoleARN == "" {
		t.Fatalf("role: %v %+v", err, *creds)
	}
	r, _ = removerOver(b, func(c *config.Config) { c.Fleet.StorageS3AccessKeyID, c.Fleet.StorageS3SecretAccessKey = "", "" })
	if err := r.RemoveObjects(ctx, refB); err == nil || !strings.Contains(err.Error(), "storage_s3_role_arn") {
		t.Fatalf("no credentials: %v", err)
	}
}
