// Package cache keeps the pipelines' caches in the container registry, beside
// the framework's image: a folder, packed as a one-layer image under a tag of
// the image's repository. Every host the pipelines may move to has a
// registry, so the caches go where the image goes, and nothing about them is
// one CI service's.
//
// The default branch writes the caches; pull requests only read them, so a
// pull request's branch can never change what another one starts from.
package cache

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/compression"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/klauspost/compress/zstd"
)

// Ref is the cache called kind for the image img, such as
// ghcr.io/owner/repo:ci-cache-test for ghcr.io/owner/repo:ci-1a2b3c.
func Ref(img, kind string) string {
	repo, _, _ := strings.Cut(img, ":")
	return repo + ":ci-cache-" + kind
}

// Restore unpacks the cache at ref into dir. A cache that is not there yet is
// not an error: the pipeline starts cold, and says so.
func Restore(ctx context.Context, ref, dir string, log io.Writer) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	img, err := remote.Image(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		var te *transport.Error
		if errors.As(err, &te) && (te.StatusCode == http.StatusNotFound || te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden) {
			fmt.Fprintf(log, "harness: no cache at %s yet; starting cold\n", ref)
			return nil
		}
		return fmt.Errorf("reading the cache %s: %w", ref, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	if len(layers) != 1 {
		return fmt.Errorf("the cache %s has %d layers, not 1", ref, len(layers))
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := untar(rc, dir); err != nil {
		return fmt.Errorf("unpacking the cache %s: %w", ref, err)
	}
	fmt.Fprintf(log, "harness: restored the cache %s\n", ref)
	return nil
}

// Save packs dir and publishes it as the cache at ref.
func Save(ctx context.Context, ref, dir string, log io.Writer) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "harness-cache-*.tar.zst")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	zw, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		f.Close()
		return err
	}
	if err := packTar(zw, dir); err != nil {
		zw.Close()
		f.Close()
		return fmt.Errorf("packing %s: %w", dir, err)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	layer, err := tarball.LayerFromFile(f.Name(), tarball.WithCompression(compression.ZStd))
	if err != nil {
		return err
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return err
	}
	// The version the tag points at now, which this save leaves untagged.
	var old string
	if desc, err := remote.Head(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)); err == nil {
		old = desc.Digest.String()
	}
	if err := remote.Write(r, img, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		return fmt.Errorf("publishing the cache %s: %w", ref, err)
	}
	if info, err := os.Stat(f.Name()); err == nil {
		fmt.Fprintf(log, "harness: saved the cache %s (%d MB)\n", ref, info.Size()>>20)
	}
	if digest, err := img.Digest(); err == nil && old != "" && old != digest.String() {
		if err := pruneGHCR(ctx, ref, old, os.Getenv("GH_TOKEN")); err != nil {
			fmt.Fprintf(log, "::warning::harness: could not delete the cache's last version, %s: %v\n", old, err)
		}
	}
	return nil
}

// packTar writes dir's folders, files and links to w, by their paths in dir.
func packTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		case info.IsDir(), info.Mode().IsRegular():
		default:
			return nil // sockets and the like are no cache's
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// untar unpacks r into dir. Folders are made writable, whatever their mode
// was, so a later run can add to them: Go keeps its module cache read-only.
func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	root := filepath.Clean(dir) + string(os.PathSeparator)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(path, root) {
			return fmt.Errorf("%s is outside the cache", h.Name)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(path, fs.FileMode(h.Mode).Perm()|0o700); err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.Remove(path)
			if err := os.Symlink(h.Linkname, path); err != nil {
				return err
			}
		case tar.TypeReg:
			os.Remove(path)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(h.Mode).Perm()|0o200)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Chtimes(path, h.ModTime, h.ModTime); err != nil {
				return err
			}
		}
	}
}
