package pail

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
	mgo "gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

type gridfsLegacyBucket struct {
	opts    GridFSOptions
	session *mgo.Session
}

func (b *gridfsLegacyBucket) normalizeKey(key string) string {
	if key == "" {
		return b.opts.Prefix
	}
	return consistentJoin(b.opts.Prefix, key)
}

func (b *gridfsLegacyBucket) denormalizeKey(key string) string {
	if b.opts.Prefix != "" && len(key) > len(b.opts.Prefix)+1 {
		key = key[len(b.opts.Prefix)+1:]
	}
	return key
}

// NewLegacyGridFSBucket creates a Bucket implementation backed by
// GridFS as implemented by the legacy "mgo" MongoDB driver. This
// constructor creates a new connection and mgo session.
//
// Mgo in general does not offer rich support for contexts, so
// cancellation may not be robust.
func NewLegacyGridFSBucket(opts GridFSOptions) (Bucket, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	if opts.MongoDBURI == "" {
		return nil, errors.New("cannot create a new bucket without a URI")
	}

	ses, err := mgo.DialWithTimeout(opts.MongoDBURI, time.Second)
	if err != nil {
		return nil, errors.Wrap(err, "problem connecting to MongoDB")
	}

	return &gridfsLegacyBucket{
		opts:    opts,
		session: ses,
	}, nil
}

// NewLegacyGridFSBucketWithSession creates a Bucket implementation
// baked by GridFS as implemented by the legacy "mgo" MongoDB driver,
// but allows you to reuse an existing session.
//
// Mgo in general does not offer rich support for contexts, so
// cancellation may not be robust.
func NewLegacyGridFSBucketWithSession(s *mgo.Session, opts GridFSOptions) (Bucket, error) {
	if s == nil {
		b, err := NewLegacyGridFSBucket(opts)
		return b, errors.WithStack(err)
	}

	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &gridfsLegacyBucket{
		opts:    opts,
		session: s,
	}, nil
}

func (b *gridfsLegacyBucket) Check(_ context.Context) error {
	if b.session == nil {
		return errors.New("no session defined")
	}

	return errors.Wrap(b.session.Ping(), "problem contacting mongodb")
}

func (b *gridfsLegacyBucket) gridFS() *mgo.GridFS {
	return b.session.DB(b.opts.Database).GridFS(b.opts.Name)
}

func (b *gridfsLegacyBucket) openFile(ctx context.Context, name string, create bool) (io.ReadWriteCloser, error) {
	ses := b.session.Clone()
	out := &legacyGridFSFile{}
	ctx, out.cancel = context.WithCancel(ctx)

	gridfs := b.gridFS()
	normalizedName := b.normalizeKey(name)

	var (
		err  error
		file *mgo.GridFile
	)

	if create {
		file, err = gridfs.Create(normalizedName)
	} else {
		file, err = gridfs.Open(normalizedName)
	}
	if err != nil {
		ses.Close()
		return nil, errors.Wrapf(err, "couldn't open %s/%s", b.opts.Name, normalizedName)
	}

	out.GridFile = file
	go func() {
		<-ctx.Done()
		ses.Close()
	}()

	return out, nil
}

type legacyGridFSFile struct {
	*mgo.GridFile
	cancel context.CancelFunc
}

func (f *legacyGridFSFile) Close() error { f.cancel(); return errors.WithStack(f.GridFile.Close()) }

func (b *gridfsLegacyBucket) Writer(ctx context.Context, name string) (io.WriteCloser, error) {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "writer",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"key":           name,
	})

	if b.opts.DryRun {
		return &mockWriteCloser{}, nil
	}
	return b.openFile(ctx, name, true)
}

func (b *gridfsLegacyBucket) Reader(ctx context.Context, name string) (io.ReadCloser, error) {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"operation":     "reader",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"key":           name,
	})

	return b.openFile(ctx, name, false)
}

func (b *gridfsLegacyBucket) Put(ctx context.Context, name string, input io.Reader) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "put",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"key":           name,
	})

	var file io.WriteCloser
	var err error
	if b.opts.DryRun {
		file = &mockWriteCloser{}
	} else {
		file, err = b.openFile(ctx, name, true)
		if err != nil {
			return errors.Wrap(err, "problem creating file")
		}
	}

	_, err = io.Copy(file, input)
	if err != nil {
		return errors.Wrap(err, "problem copying data")
	}

	return errors.Wrap(file.Close(), "problem flushing data to file")
}

func (b *gridfsLegacyBucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"operation":     "get",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"key":           name,
	})

	return b.Reader(ctx, name)
}

func (b *gridfsLegacyBucket) Upload(ctx context.Context, name, path string) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "upload",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"key":           name,
		"path":          path,
	})

	f, err := os.Open(path)
	if err != nil {
		return errors.Wrapf(err, "problem opening file %s", name)
	}
	defer f.Close()

	return errors.WithStack(b.Put(ctx, name, f))
}

func (b *gridfsLegacyBucket) Download(ctx context.Context, name, path string) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"operation":     "download",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"key":           name,
		"path":          path,
	})

	reader, err := b.Reader(ctx, name)
	if err != nil {
		return errors.WithStack(err)
	}

	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.Wrapf(err, "problem creating enclosing directory for '%s'", path)
	}

	f, err := os.Create(path)
	if err != nil {
		return errors.Wrapf(err, "problem creating file '%s'", path)
	}
	_, err = io.Copy(f, reader)
	if err != nil {
		_ = f.Close()
		return errors.Wrap(err, "problem copying data")
	}

	return errors.WithStack(f.Close())
}

func (b *gridfsLegacyBucket) Push(ctx context.Context, opts SyncOptions) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "push",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"remote":        opts.Remote,
		"local":         opts.Local,
		"exclude":       opts.Exclude,
	})

	var re *regexp.Regexp
	var err error
	if opts.Exclude != "" {
		re, err = regexp.Compile(opts.Exclude)
		if err != nil {
			return errors.Wrap(err, "problem compiling exclude regex")
		}
	}

	localPaths, err := walkLocalTree(ctx, opts.Local)
	if err != nil {
		return errors.Wrap(err, "problem finding local paths")
	}

	gridfs := b.gridFS()
	for _, path := range localPaths {
		if re != nil && re.MatchString(path) {
			continue
		}

		target := consistentJoin(opts.Remote, path)
		file, err := gridfs.Open(b.normalizeKey(target))
		if err == mgo.ErrNotFound {
			if err = b.Upload(ctx, target, filepath.Join(opts.Local, path)); err != nil {
				return errors.Wrapf(err, "problem uploading '%s' to '%s'", path, target)
			}
			continue
		} else if err != nil {
			return errors.Wrapf(err, "problem finding '%s'", target)
		}

		localmd5, err := md5sum(filepath.Join(opts.Local, path))
		if err != nil {
			return errors.Wrapf(err, "problem checksumming '%s'", path)
		}

		if file.MD5() != localmd5 {
			if err = b.Upload(ctx, target, filepath.Join(opts.Local, path)); err != nil {
				return errors.Wrapf(err, "problem uploading '%s' to '%s'", path, target)
			}
		}
	}

	if (b.opts.DeleteOnPush || b.opts.DeleteOnSync) && !b.opts.DryRun {
		return errors.Wrap(deleteOnPush(ctx, localPaths, opts.Remote, b), "problem with delete on sync after push")
	}
	return nil
}

func (b *gridfsLegacyBucket) Pull(ctx context.Context, opts SyncOptions) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"operation":     "pull",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"remote":        opts.Remote,
		"local":         opts.Local,
		"exclude":       opts.Exclude,
	})

	var re *regexp.Regexp
	var err error
	if opts.Exclude != "" {
		re, err = regexp.Compile(opts.Exclude)
		if err != nil {
			return errors.Wrap(err, "problem compiling exclude regex")
		}
	}

	iter, err := b.List(ctx, opts.Remote)
	if err != nil {
		return errors.WithStack(err)
	}

	iterimpl, ok := iter.(*legacyGridFSIterator)
	if !ok {
		return errors.New("programmer error")
	}

	gridfs := b.gridFS()
	var f *mgo.GridFile
	var checksum string
	keys := []string{}
	for gridfs.OpenNext(iterimpl.iter, &f) {
		if re != nil && re.MatchString(f.Name()) {
			continue
		}

		denormalizedName := b.denormalizeKey(f.Name())
		fn := denormalizedName[len(opts.Remote)+1:]
		name := filepath.Join(opts.Local, fn)
		keys = append(keys, fn)
		checksum, err = md5sum(name)
		if os.IsNotExist(errors.Cause(err)) {
			if err = b.Download(ctx, denormalizedName, name); err != nil {
				return errors.WithStack(err)
			}
			continue
		} else if err != nil {
			return errors.WithStack(err)
		}

		// NOTE: it doesn't seem like the md5 sums are being
		// populated, so this always happens
		if f.MD5() != checksum {
			if err = b.Download(ctx, denormalizedName, name); err != nil {
				return errors.WithStack(err)
			}
		}
	}

	if err = iterimpl.iter.Err(); err != nil {
		return errors.Wrap(err, "problem iterating bucket")
	}

	if (b.opts.DeleteOnPull || b.opts.DeleteOnSync) && !b.opts.DryRun {
		return errors.Wrap(deleteOnPull(ctx, keys, opts.Local), "problem with delete on sync after pull")
	}
	return nil
}

func (b *gridfsLegacyBucket) Copy(ctx context.Context, options CopyOptions) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"operation":     "copy",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"source_key":    options.SourceKey,
		"dest_key":      options.DestinationKey,
	})

	from, err := b.Reader(ctx, options.SourceKey)
	if err != nil {
		return errors.Wrap(err, "problem getting reader for source")
	}

	to, err := options.DestinationBucket.Writer(ctx, options.DestinationKey)
	if err != nil {
		return errors.Wrap(err, "problem getting writer for destination")
	}

	if _, err = io.Copy(to, from); err != nil {
		return errors.Wrap(err, "problem copying data")
	}

	return errors.WithStack(to.Close())
}

func (b *gridfsLegacyBucket) Remove(ctx context.Context, key string) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "remove",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"key":           key,
	})

	if b.opts.DryRun {
		return nil
	}
	return errors.Wrapf(b.gridFS().Remove(b.normalizeKey(key)), "problem removing file %s", key)
}

func (b *gridfsLegacyBucket) RemoveMany(ctx context.Context, keys ...string) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "remove many",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"keys":          keys,
	})

	catcher := grip.NewBasicCatcher()
	for _, key := range keys {
		catcher.Add(b.Remove(ctx, key))
	}
	return catcher.Resolve()
}

func (b *gridfsLegacyBucket) RemovePrefix(ctx context.Context, prefix string) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "remove prefix",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"prefix":        prefix,
	})

	return removePrefix(ctx, prefix, b)
}

func (b *gridfsLegacyBucket) RemoveMatching(ctx context.Context, expression string) error {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"dry_run":       b.opts.DryRun,
		"operation":     "remove matching",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"expression":    expression,
	})

	return removeMatching(ctx, expression, b)
}

func (b *gridfsLegacyBucket) List(ctx context.Context, prefix string) (BucketIterator, error) {
	grip.DebugWhen(b.opts.Verbose, message.Fields{
		"type":          "legacy_gridfs",
		"operation":     "list",
		"bucket":        b.opts.Name,
		"bucket_prefix": b.opts.Prefix,
		"prefix":        prefix,
	})

	if ctx.Err() != nil {
		return nil, errors.New("operation canceled")
	}

	if prefix == "" {
		return &legacyGridFSIterator{
			ctx:    ctx,
			iter:   b.gridFS().Find(nil).Iter(),
			bucket: b,
		}, nil
	}

	return &legacyGridFSIterator{
		ctx:    ctx,
		iter:   b.gridFS().Find(bson.M{"filename": bson.RegEx{Pattern: fmt.Sprintf("^%s.*", b.normalizeKey(prefix))}}).Iter(),
		bucket: b,
	}, nil
}

type legacyGridFSIterator struct {
	ctx    context.Context
	err    error
	item   *bucketItemImpl
	bucket *gridfsLegacyBucket
	iter   *mgo.Iter
}

func (iter *legacyGridFSIterator) Err() error       { return iter.err }
func (iter *legacyGridFSIterator) Item() BucketItem { return iter.item }

func (iter *legacyGridFSIterator) Next(ctx context.Context) bool {
	if iter.ctx.Err() != nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}

	var f *mgo.GridFile

	gridfs := iter.bucket.gridFS()

	if !gridfs.OpenNext(iter.iter, &f) {
		return false
	}

	iter.item = &bucketItemImpl{
		bucket: iter.bucket.opts.Prefix,
		key:    iter.bucket.denormalizeKey(f.Name()),
		b:      iter.bucket,
	}

	return true
}
