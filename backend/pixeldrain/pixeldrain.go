// Package pixeldrain provides an interface to the Pixeldrain object storage
// system.
package pixeldrain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/rest"
)

const (
	filesystemEndpoint = "/filesystem"
	userEndpoint       = "/user"
	logRequests        = true
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "pixeldrain",
		Description: "Pixeldrain Filesystem",
		NewFs:       NewFs,
		Config:      nil,
		Options: []fs.Option{{
			Name: "api_key",
			Help: "API key for your pixeldrain account\n" +
				"Found on https://pixeldrain.com/user/api_keys.",
			Sensitive: true,
		}, {
			Name: "bucket_id",
			Help: "Root of the filesystem to use. Set to 'me' to use your personal filesystem.\n" +
				"Set to a shared directory ID to use a shared directory.",
			Default: "me",
		}, {
			Name: "api_url",
			Help: "The API endpoint to connect to. In the vast majority of cases it's fine to leave\n" +
				"this at default. It is only intended to be changed for testing purposes.",
			Default:  "https://pixeldrain.com/api",
			Advanced: true,
			Required: true,
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	APIKey   string `config:"api_key"`
	BucketID string `config:"bucket_id"`
	APIURL   string `config:"api_url"`
}

// ItemMeta defines metadata we cache for each Item ID
type ItemMeta struct {
	SequenceID int64  // the most recent event processed for this item
	ParentID   string // ID of the parent directory of this item
	Name       string // leaf name of this item
}

// Fs represents a remote box
type Fs struct {
	name     string       // name of this remote
	root     string       // the path we are working on
	opt      Options      // parsed options
	features *fs.Features // optional features
	srv      *rest.Client // the connection to the server
	loggedIn bool         // if the user is authenticated

	// Pathprefix is the directory we're working in. The pathPrefix is stripped
	// from every API response containing a path. The pathPrefix must start with
	// a slash because the API also starts each path with a slash
	pathPrefix string
}

// Object describes a pixeldrain file
//
// Will definitely have info but maybe not meta
type Object struct {
	fs *Fs // what this object is part of

	// Points to a node in the path slice
	base     *FilesystemNode
	path     []FilesystemNode
	children []FilesystemNode
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
		srv:  rest.NewClient(fshttp.NewClient(ctx)).SetErrorHandler(apiErrorHandler),
	}
	f.features = (&fs.Features{
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	f.pathPrefix = "/" + opt.BucketID + "/"
	if root != "" {
		f.pathPrefix += root + "/"
	}
	f.srv.SetRoot(opt.APIURL + filesystemEndpoint + f.pathPrefix)

	// If using an accessToken, set the Authorization header
	if len(opt.APIKey) > 1 {
		f.srv.SetUserPass("", opt.APIKey)

		// Check if credentials are correct
		user, err := f.userInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get user data: %w", err)
		}

		f.loggedIn = true

		fs.Infof(nil,
			"Logged in as '%s', subscription '%s', storage limit %d\n",
			user.Username, user.Subscription.Name, user.Subscription.StorageSpace,
		)
	}

	if !f.loggedIn && opt.BucketID == "me" {
		return nil, errors.New("authentication required: the 'me' directory can only be accessed while logged in")
	}

	fs.Infof(nil,
		"Created filesystem with name '%s', root '%s', bucket '%s', endpoint '%s'\n",
		name, root, opt.BucketID, opt.APIURL+filesystemEndpoint+f.pathPrefix,
	)

	return f, nil
}

func logRequest(str string, args ...any) {
	if logRequests {
		fmt.Printf(str+"\n", args...)
	}
}

// =================================
// Implementation of fs.FS interface
// =================================
var _ fs.Fs = (*Fs)(nil)

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	logRequest("List '%s'", dir)

	fsp, err := f.stat(ctx, dir)
	if err == errNotFound {
		return entries, fs.ErrorDirNotFound
	} else if err != nil {
		return entries, err
	}

	entries = make(fs.DirEntries, len(fsp.Children))
	for i := range fsp.Children {
		if fsp.Children[i].Type == "dir" {
			entries[i] = f.nodeToDirectory(fsp.Children[i])
		} else {
			entries[i] = f.nodeToObject(fsp.Children[i])
		}
	}

	return entries, nil
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	logRequest("NewObject '%s'", remote)

	fsp, err := f.stat(ctx, remote)
	if err == errNotFound {
		logRequest("Object '%s' does not exist", remote)
		return nil, fs.ErrorObjectNotFound
	} else if err != nil {
		return nil, err
	} else if fsp.Path[fsp.BaseIndex].Type == "dir" {
		return nil, fs.ErrorIsDir
	}
	return f.pathToObject(fsp), nil
}

// Put the object
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	logRequest("Put '%s'", src.Remote())

	_, err := f.put(ctx, src.Remote(), in, options)
	if err != nil {
		return nil, fmt.Errorf("failed to put object: %w", err)
	}

	// Can't set modtime in the same request
	fsp, err := f.update(
		ctx, src.Remote(),
		map[string]any{"modified": src.ModTime(ctx)},
	)
	if err != nil {
		return nil, err
	}

	return f.nodeToObject(fsp), nil
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) (err error) {
	logRequest("Mkdir '%s'", dir)

	err = f.mkdir(ctx, dir)
	if err == errNotFound {
		return fs.ErrorDirNotFound
	} else if err == errExists {
		// Spec says we do not return an error if the directory already exists
		return nil
	}
	return err
}

// Rmdir deletes the root folder
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) (err error) {
	logRequest("Rmdir '%s'", dir)

	err = f.delete(ctx, dir, false)
	if err == errNotFound {
		return fs.ErrorDirNotFound
	}
	return err
}

// ===================================
// Implementation of fs.Info interface
// ===================================
var _ fs.Info = (*Fs)(nil)

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string { return f.name }

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string { return f.root }

// String converts this Fs to a string
func (f *Fs) String() string { return fmt.Sprintf("pixeldrain root '%s'", f.root) }

// Precision return the precision of this Fs
func (f *Fs) Precision() time.Duration { return time.Millisecond }

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set { return hash.Set(hash.SHA256) }

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features { return f.features }

// ====================================
// Implementation of fs.Purger interface
// ====================================
var _ fs.Purger = (*Fs)(nil)

// Purge all files in the directory specified
//
// Implement this if you have a way of deleting all the files
// quicker than just running Remove() on the result of List()
//
// Return an error if it doesn't exist
func (f *Fs) Purge(ctx context.Context, dir string) (err error) {
	logRequest("Purge '%s'", dir)

	err = f.delete(ctx, dir, true)
	if err == errNotFound {
		return fs.ErrorDirNotFound
	}
	return err
}

// ====================================
// Implementation of fs.Mover interface
// ====================================
var _ fs.Mover = (*Fs)(nil)

// Move src to this remote using server-side move operations.
//
// This is stored with the remote path given.
//
// It returns the destination Object and a possible error.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantMove
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	logRequest("Move '%s' '%s'", src.Remote(), remote)

	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}

	err := f.rename(ctx, src.Remote(), remote)
	if err == errNotFound {
		return nil, fs.ErrorCantMove
	} else if err != nil {
		return nil, fmt.Errorf("failed to rename file: %w", err)
	}

	srcObj.base.Path = remote
	return srcObj, nil
}

// =======================================
// Implementation of fs.DirMover interface
// =======================================
var _ fs.DirMover = (*Fs)(nil)

// Move src to this remote using server-side move operations.
//
// This is stored with the remote path given.
//
// It returns the destination Object and a possible error.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantMove
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) (err error) {
	logRequest("DirMove '%s' '%s'", srcRemote, dstRemote)

	err = f.rename(ctx, srcRemote, dstRemote)
	if err == errNotFound {
		return fs.ErrorDirNotFound
	} else if err == errExists {
		return fs.ErrorDirExists
	}

	return err
}

// ======================================
// Implementation of fs.Abouter interface
// ======================================
var _ fs.Abouter = (*Fs)(nil)

// About gets quota information
func (f *Fs) About(ctx context.Context) (usage *fs.Usage, err error) {
	logRequest("About")

	user, err := f.userInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read user info: %w", err)
	}

	if user.Subscription.StorageSpace == -1 {
		user.Subscription.StorageSpace = 1e15 // 1 PB
	}

	return &fs.Usage{
		Used:  fs.NewUsageValue(user.StorageSpaceUsed),
		Total: fs.NewUsageValue(user.Subscription.StorageSpace),
		Free:  fs.NewUsageValue(user.StorageSpaceUsed - user.Subscription.StorageSpace),
	}, nil
}

// =====================================
// Implementation of fs.Object interface
// =====================================
var _ fs.Object = (*Object)(nil)

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) (err error) {
	logRequest("SetModTime '%s'", o.base.Path)

	_, err = o.fs.update(ctx, o.base.Path, map[string]any{"modified": modTime})
	if err == nil {
		o.base.Modified = modTime
	}
	return err
}

// Open an object for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	logRequest("Open '%s'", o.base.Path)

	return o.fs.read(ctx, o.base.Path, options)
}

// Update the object with the contents of the io.Reader, modTime and size
//
// If existing is set then it updates the object rather than creating a new one.
//
// The new object may have been created if an error is returned.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	logRequest("Update '%s' '%d'", src.Remote(), src.Size())

	newObj, err := o.fs.Put(ctx, in, src, options...)
	if err == nil {
		// We replace our object with the new object so we don't have to copy
		// all the updated values
		*o = *newObj.(*Object)
	}
	return err
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	logRequest("Remove '%s'", o.base.Path)

	return o.fs.delete(ctx, o.base.Path, false)
}

// =========================================
// Implementation of fs.ObjectInfo interface
// =========================================
var _ fs.ObjectInfo = (*Object)(nil)

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Hash returns the SHA-256 of an object returning a lowercase hex string
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.SHA256 {
		return "", hash.ErrUnsupported
	}
	return o.base.SHA256Sum, nil
}

// Storable returns a boolean showing whether this object storable
func (o *Object) Storable() bool {
	return true
}

// =======================================
// Implementation of fs.DirEntry interface
// =======================================
var _ fs.DirEntry = (*Object)(nil)

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.base.Path
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.base.Path
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.base.Modified
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.base.FileSize
}

// ========================================
// Implementation of fs.MimeTyper interface
// ========================================
var _ fs.MimeTyper = (*Object)(nil)

func (o *Object) MimeType(ctx context.Context) string {
	return o.base.FileType
}
