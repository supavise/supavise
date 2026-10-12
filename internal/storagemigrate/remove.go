package storagemigrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/secrets"
)

// removeBatch is how many keys RemoveObjects deletes at a time.
const removeBatch = 500

// ObjectRemover removes the Storage objects of a project from the bucket of [fleet] storage_backend =
// "s3": every key under <ref>/. The Engine calls it when a project is deleted and when a branch is
// reset (lifecycle.ObjectRemover), because the S3 backend keeps objects outside the project's
// directory and nothing else would ever delete them. It reads the bucket and the credentials from the
// [fleet] settings the way Storage does: a static key or the role the daemon assumes. A node with
// neither (the AWS default chain of Storage's own environment) cannot be reached from here, and the
// call says so.
type ObjectRemover struct {
	Cfg *config.Config
	// Open connects to the bucket; nil is OpenS3. Tests give a fake.
	Open OpenFunc
}

// NewObjectRemover returns the remover of cfg's bucket.
func NewObjectRemover(cfg *config.Config) *ObjectRemover { return &ObjectRemover{Cfg: cfg} }

// RemoveObjects deletes the objects of ref. A backend other than S3 has none to delete.
func (r *ObjectRemover) RemoveObjects(ctx context.Context, ref string) error {
	f := r.Cfg.Fleet
	if f.StorageBackend != "s3" {
		return nil
	}
	if !secrets.ValidRef(ref) { // the prefix must never be empty or reach another project
		return fmt.Errorf("storagemigrate: refusing to remove the objects of %q", ref)
	}
	if f.StorageS3RoleARN == "" && (f.StorageS3AccessKeyID == "" || f.StorageS3SecretAccessKey == "") {
		return errors.New("storagemigrate: [fleet] has neither a static access key nor storage_s3_role_arn, so the daemon cannot open the bucket to remove the project's objects")
	}
	open := r.Open
	if open == nil {
		open = OpenS3
	}
	b, err := open(ctx,
		Destination{Bucket: f.StorageS3Bucket, Endpoint: f.StorageS3Endpoint, Region: f.StorageS3Region, PathStyle: f.StorageS3ForcePathStyle},
		Credentials{Source: CredConfig, AccessKeyID: f.StorageS3AccessKeyID, SecretAccessKey: f.StorageS3SecretAccessKey, RoleARN: f.StorageS3RoleARN})
	if err != nil {
		return err
	}
	var keys []string
	flush := func() error {
		if len(keys) == 0 {
			return nil
		}
		err := b.Delete(ctx, keys...)
		keys = keys[:0]
		return err
	}
	if err := b.List(ctx, ref+"/", func(en Entry) error {
		keys = append(keys, en.Key)
		if len(keys) >= removeBatch {
			return flush()
		}
		return nil
	}); err != nil {
		return fmt.Errorf("storagemigrate: list the objects of %s: %w", ref, err)
	}
	return flush()
}
