//go:build ignore

package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) <= 1 {
		os.Exit(2)
	}
	for _, fw := range os.Args[1:] {
		if err := generate(fw); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", fw, err)
			os.Exit(1)
		}
	}
}

func generate(fw string) error {
	if !strings.Contains(fw, "/") && !strings.ContainsRune(fw, filepath.Separator) {
		u, err := resolve(fw)
		if err != nil {
			return fmt.Errorf("resolve firmware version %q: %w", fw, err)
		}
		fw = u
	}
	fmt.Printf("reading %q\n", fw)

	version, ok := cutVersion(fw)
	if !ok {
		return fmt.Errorf("failed to extract firmware version from name %q", version)
	}

	var (
		updateZipReader io.ReaderAt
		updateZipSize   int64
	)
	if strings.Contains(fw, "://") {
		r, err := httpGetAt(fw)
		if err != nil {
			return fmt.Errorf("fetch firmware: %w", err)
		}
		updateZipReader, updateZipSize = r, r.Size()
	} else {
		f, err := os.Open(fw)
		if err != nil {
			return fmt.Errorf("open firmware: %w", err)
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			return fmt.Errorf("open firmware: %w", err)
		}
		updateZipReader, updateZipSize = f, fi.Size()
	}

	updateZip, err := zip.NewReader(updateZipReader, updateZipSize)
	if err != nil {
		return fmt.Errorf("open firmware: %w", err)
	}

	var koboRootGz io.Reader
	for _, fh := range updateZip.File {
		if fh.Name == "KoboRoot.tgz" {
			offset, err := fh.DataOffset()
			if err != nil {
				return fmt.Errorf("open firmware: get tgz offset: %w", err)
			}
			size := int64(fh.CompressedSize64)

			if r, ok := updateZipReader.(*httpReaderAt); ok {
				r, err := r.NewSectionReader(offset, size)
				if err != nil {
					return fmt.Errorf("open firmware: open KoboRoot.tgz: %w", err)
				}
				defer r.Close()
				koboRootGz = r
			} else {
				koboRootGz = io.NewSectionReader(updateZipReader, offset, size)
			}

			switch fh.Method {
			case zip.Store:
			case zip.Deflate:
				koboRootGz = flate.NewReader(koboRootGz)
			default:
				return fmt.Errorf("open firmware: unsupported mothod %d", fh.Method)
			}
			break
		}
	}
	if koboRootGz == nil {
		return fmt.Errorf("open firmware: missing KoboRoot.tgz")
	}

	koboRootTar, err := gzip.NewReader(koboRootGz)
	if err != nil {
		return fmt.Errorf("open firmware: %w", err)
	}

	files := map[string]bool{
		"usr/local/Kobo/fontickel":          false,
		"usr/local/Kobo/libadobe.so":        false,
		"usr/local/Kobo/libnickel.so.1.0.0": false,
		"usr/local/Kobo/librmsdk.so.1.0.0":  false,
		"usr/local/Kobo/nickel":             false,
		"usr/local/Kobo/sickel":             false,
		"usr/local/Kobo/strickel":           false,
		"usr/local/Kobo/revinfo":            false,
	}

	outFile, err := os.CreateTemp(".", ".testdata-")
	if err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	defer func() {
		if outFile != nil {
			outFile.Close()
			if err := os.Remove(outFile.Name()); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to remove %q: %v\n", outFile.Name(), err)
			}
		}
	}()

	xz := exec.Command("xz", "-9", "-c") // native xz has better compression and performance
	xz.Stdout = outFile
	xz.Stderr = os.Stderr
	outFileXz, err := xz.StdinPipe()
	if err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := xz.Start(); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	out := tar.NewWriter(outFileXz)

	t := tar.NewReader(koboRootTar)
	for {
		h, err := t.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read firmware: %w", err)
		}

		name := path.Clean(h.Name)
		seen, want := files[name]
		if !want {
			continue
		}
		if seen {
			return fmt.Errorf("duplicate files %q", name)
		}
		files[name] = true

		if mode := h.FileInfo().Mode(); !mode.IsRegular() {
			return fmt.Errorf("file %q has non-regular mode %s", name, mode)
		}

		if err := out.WriteHeader(&tar.Header{
			Name:    path.Base(name),
			ModTime: h.ModTime,
			Mode:    0644,
			Size:    h.Size,
		}); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		if _, err := io.Copy(out, t); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}

	for name, have := range files {
		if !have {
			fmt.Fprintf(os.Stderr, "warning: %q: missing %q\n", fw, name)
		}
	}

	fmt.Printf("finishing up\n")
	if err := out.Close(); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := outFileXz.Close(); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := xz.Wait(); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := os.Chmod(outFile.Name(), 0644); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := os.Rename(outFile.Name(), version+".tar.xz"); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	outFile = nil

	fmt.Printf("generated %s.tar.xz\n", version)
	return nil
}

func resolve(version string) (string, error) {
	resp, err := http.Get("https://kfw.storage.pgaskin.net/MD5SUMS")
	if err != nil {
		return "", fmt.Errorf("fetch mirrored firmware list: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch mirrored firmware list: response status %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		_, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			return "", fmt.Errorf("invalid md5sums line %q", line)
		}
		if !strings.HasSuffix(name, version) {
			if tmp, ok := cutVersion(name); !ok || tmp != version {
				continue
			}
		}
		return "https://kfw.storage.pgaskin.net/" + path.Clean(name), nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("fetch mirrored firmware list: %w", err)
	}
	return "", fmt.Errorf("no mirrored firmware file matching %q", version)
}

func cutVersion(name string) (string, bool) {
	_, version, _ := strings.Cut(name, "kobo-update-")
	version = version[:len(version)-len(strings.TrimLeft(version, "0123456789."))]
	version = strings.Trim(version, ".")
	return version, version != ""
}

func httpGetAt(u string) (*httpReaderAt, error) {
	req, err := http.NewRequest(http.MethodHead, u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("response status %d", resp.StatusCode)
	}

	if resp.Header.Get("Accept-Ranges") != "bytes" {
		return nil, fmt.Errorf("server does not support range requests")
	}

	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid or imssing content length")
	}

	return &httpReaderAt{
		url:  resp.Request.URL.String(),
		size: size,
	}, nil
}

type httpReaderAt struct {
	url  string
	size int64
}

func (r *httpReaderAt) Size() int64 {
	return r.size
}

func (r *httpReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	var (
		start = off
		end   = off + int64(len(p)-1)
	)
	if start < 0 {
		return 0, errors.New("invalid offset")
	}
	if end <= start {
		return 0, nil
	}
	if start >= r.size {
		return 0, io.ErrUnexpectedEOF
	}

	req, err := http.NewRequest(http.MethodGet, r.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("response status %d", resp.StatusCode)
	}

	return io.ReadFull(io.LimitReader(resp.Body, int64(len(p))), p)
}

func (r *httpReaderAt) NewSectionReader(off, n int64) (io.ReadCloser, error) {
	if off < 0 || n < 0 {
		return nil, errors.New("invalid offset")
	}
	if n == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	req, err := http.NewRequest(http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("response status %d", resp.StatusCode)
	}

	rc := &readerCloser{resp.Body, resp.Body}
	if n < 1<<63-1 {
		rc.Reader = io.LimitReader(rc.Reader, n)
	}
	return rc, nil
}

type readerCloser struct {
	io.Reader
	io.Closer
}
