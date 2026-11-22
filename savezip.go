package main

// Save packages for platforms whose save is a DIRECTORY (PSP: PSP/SAVEDATA/<GAMEID>/..., later PS2 memory cards): one .zip, as the
// Totv's contract wants (docs/games-streaming.md §5, validateSaveZip in server/games/saves.js).
//
// The package is written and read HERE, in the parent, never by the emulator child: the child only has a read-write Landlock
// rule on its own save directory and gets no zip code to attack. The zip is created after the child exited (or, for the periodic
// upload, from files that have been quiet for a moment), and a zip downloaded from the Totv is validated again before a single
// byte is written to disk (the Totv's own validation is not trusted).
//
// Format (what validateSaveZip accepts, and what is produced): classic zip (no ZIP64, so every size < 4 GiB and <= 65534 entries),
// methods stored(0) or deflate(8), no encryption, names relative with '/' only, no "..", no leading '/', no drive letters, no
// backslash, no NUL, the local header name equals the central directory name, no symlinks (Unix mode in the external attributes).
// The component is stricter than the Totv in two places: names must start with the allowed prefix (PSP/SAVEDATA/), and the same
// name may not appear twice.

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"
)

// saveLimits bound a save package. The Totv accepts up to limit*4 unpacked (PSP: 64 MiB * 4 = 256 MiB) and
// "256 MB per entry" for the PSP.
type saveLimits struct {
	MaxFiles    int   // entries
	MaxEntry    int64 // unpacked bytes of one entry
	MaxUnpacked int64 // unpacked bytes of the whole package
	MaxPacked   int64 // size of the .zip itself (the Totv's saveLimits.sram for the platform)
}

var pspSaveLimits = saveLimits{MaxFiles: 4096, MaxEntry: 256 << 20, MaxUnpacked: 256 << 20, MaxPacked: 64 << 20}

// pspSavePrefix is the only part of the memory stick that is a save: the rest of the directory (PSP/SYSTEM/CACHE: shader
// cache, PSP/GAME, ppsspp.ini...) is never packed and never restored (a hostile zip could otherwise plant a homebrew in PSP/GAME).
const pspSavePrefix = "PSP/SAVEDATA/"

var errSaveZip = errors.New("invalid save zip")

func badZip(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errSaveZip, fmt.Sprintf(format, a...))
}

// zipNameShape reports whether an entry name is harmless as a path (the zip-slip family): relative, '/' only, no "." / ".." / empty
// component, no drive letter, no NUL or control characters, no backslash. A trailing '/' (a directory entry) is allowed here.
func zipNameShape(name string) bool {
	if name == "" || len(name) > 512 || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return false
	}
	if len(name) >= 2 && name[1] == ':' && (name[0]|0x20) >= 'a' && (name[0]|0x20) <= 'z' {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, p := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if p == ".." || p == "." || p == "" {
			return false
		}
	}
	return true
}

// safeZipName reports whether a FILE name is acceptable to pack or extract: a harmless shape (zipNameShape), not a directory entry,
// below the allowed prefix.
func safeZipName(name, prefix string) bool {
	return zipNameShape(name) && !strings.HasSuffix(name, "/") && strings.HasPrefix(name, prefix) && len(name) > len(prefix)
}

// zipSaveDir packs <root>/<prefix...> into a zip. Only regular files are taken (symlinks and other types are skipped, never
// followed); limits are enforced while reading, so a file that grows during the walk cannot exceed them. Files whose mtime is
// newer than `quiet` ago are a sign that the core is still writing: errSaveBusy is returned and the caller tries again later.
var errSaveBusy = errors.New("save files still being written")

// walkSaveFiles lists the regular files below dir (relative to root, '/' separated) without ever following a symlink: directories are
// opened with O_NOFOLLOW|O_DIRECTORY through the os.Root (which also refuses any path that would leave it), links and special files
// are skipped.
func walkSaveFiles(root *os.Root, dir string, fn func(name string, info fs.FileInfo) error) error {
	d, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return nil // no savedata yet (or the directory is not a directory): nothing to pack
		}
		return err
	}
	ents, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, e := range ents {
		name := dir + "/" + e.Name()
		switch t := e.Type(); {
		case t&fs.ModeSymlink != 0:
		case e.IsDir():
			if err := walkSaveFiles(root, name, fn); err != nil {
				return err
			}
		case t.IsRegular():
			info, err := e.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // removed while we looked
				}
				return err
			}
			if err := fn(name, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func zipSaveDir(rootDir, prefix string, lim saveLimits, quiet time.Duration) ([]byte, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var names []string
	var total int64
	now := time.Now()
	err = walkSaveFiles(root, strings.TrimSuffix(prefix, "/"), func(name string, info fs.FileInfo) error {
		if quiet > 0 && now.Sub(info.ModTime()) < quiet {
			return errSaveBusy
		}
		if !safeZipName(name, prefix) {
			return nil // a file the format cannot carry (odd name): left out, never renamed
		}
		if info.Size() > lim.MaxEntry {
			return badZip("%q is larger than %d bytes", name, lim.MaxEntry)
		}
		total += info.Size()
		if total > lim.MaxUnpacked {
			return badZip("save larger than %d bytes", lim.MaxUnpacked)
		}
		if len(names) >= lim.MaxFiles {
			return badZip("more than %d files", lim.MaxFiles)
		}
		names = append(names, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil // no save yet
	}
	sort.Strings(names) // same bytes for the same content order: the "did it change" comparison stays meaningful
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var read int64
	for _, name := range names {
		// O_NOFOLLOW: a file swapped for a link after the walk is refused; O_NONBLOCK: one swapped for a FIFO cannot block the open;
		// the descriptor itself must then be a regular file (fstat), whatever the name was a moment ago
		f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
				continue
			}
			return nil, err
		}
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			f.Close()
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Unix(0, 0).UTC()})
		if err != nil {
			f.Close()
			return nil, err
		}
		n, err := io.Copy(w, io.LimitReader(f, lim.MaxEntry+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		read += n
		if n > lim.MaxEntry || read > lim.MaxUnpacked {
			return nil, badZip("save grew past the limit while reading")
		}
		if int64(buf.Len()) > lim.MaxPacked {
			return nil, badZip("package larger than %d bytes", lim.MaxPacked)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > lim.MaxPacked {
		return nil, badZip("package larger than %d bytes", lim.MaxPacked)
	}
	return buf.Bytes(), nil
}

type zipEntry struct {
	name         string
	method       uint16
	csize, usize uint32
	dataOff      int64 // start of the compressed data (after the local header)
}

// parseSaveZip validates a zip held in memory (a save is at most MaxPacked, 64 MiB) the way server/games/saves.js does,
// reading the central directory itself because archive/zip does not compare the local header with it. Nothing is decompressed
// here except through inflateExact, which bounds the real size.
func parseSaveZip(z []byte, prefix string, lim saveLimits) ([]zipEntry, error) {
	size := int64(len(z))
	if size < 22 {
		return nil, badZip("too small")
	}
	if size > lim.MaxPacked {
		return nil, badZip("package larger than %d bytes", lim.MaxPacked)
	}
	tail := int64(0)
	if size-22-0xffff > 0 {
		tail = size - 22 - 0xffff
	}
	e := -1
	for i := len(z) - 22; i >= int(tail); i-- {
		if binary.LittleEndian.Uint32(z[i:]) == 0x06054b50 {
			e = i
			break
		}
	}
	if e < 0 {
		return nil, badZip("not a zip")
	}
	n := int(binary.LittleEndian.Uint16(z[e+10:]))
	cdSize := int64(binary.LittleEndian.Uint32(z[e+12:]))
	cdOff := int64(binary.LittleEndian.Uint32(z[e+16:]))
	if n == 0xffff || cdSize == 0xffffffff || cdOff == 0xffffffff {
		return nil, badZip("zip64 not supported")
	}
	if n == 0 || n > lim.MaxFiles || cdSize > 8<<20 || cdOff+cdSize > int64(e) {
		return nil, badZip("bad directory")
	}
	cd := z[cdOff : cdOff+cdSize]
	var entries []zipEntry
	seen := map[string]bool{}
	kinds := map[string]bool{} // path without the trailing '/' -> is a directory entry
	var total int64
	p := 0
	for k := 0; k < n; k++ {
		if p+46 > len(cd) || binary.LittleEndian.Uint32(cd[p:]) != 0x02014b50 {
			return nil, badZip("bad directory")
		}
		flags := binary.LittleEndian.Uint16(cd[p+8:])
		method := binary.LittleEndian.Uint16(cd[p+10:])
		csize := binary.LittleEndian.Uint32(cd[p+20:])
		usize := binary.LittleEndian.Uint32(cd[p+24:])
		nameLen := int(binary.LittleEndian.Uint16(cd[p+28:]))
		extraLen := int(binary.LittleEndian.Uint16(cd[p+30:]))
		commentLen := int(binary.LittleEndian.Uint16(cd[p+32:]))
		relOff := int64(binary.LittleEndian.Uint32(cd[p+42:]))
		if csize == 0xffffffff || usize == 0xffffffff || uint32(relOff) == 0xffffffff {
			return nil, badZip("zip64 not supported")
		}
		if p+46+nameLen+extraLen+commentLen > len(cd) {
			return nil, badZip("bad directory")
		}
		next := p + 46 + nameLen + extraLen + commentLen
		nameBytes := cd[p+46 : p+46+nameLen]
		name := string(nameBytes)
		host := binary.LittleEndian.Uint16(cd[p+4:]) >> 8
		mode := binary.LittleEndian.Uint32(cd[p+38:]) >> 16
		// THE rule (README, "Hardening (revisao 2)"): the package is REFUSED for what is dangerous (a name that could leave the directory,
		// a symlink anywhere in it, sizes over the limits, a lying header, duplicate names, encryption/unknown compression in an entry that
		// would be extracted). What is merely not ours is IGNORED, never extracted: directory entries and names outside the prefix.
		if host == 3 && mode&0o170000 == 0o120000 {
			return nil, badZip("symlink")
		}
		if !zipNameShape(name) {
			return nil, badZip("unsafe name")
		}
		// a name may appear once, and "a/" next to "a" is the same path as directory and file: refused for every entry, ignored or not
		isDir := strings.HasSuffix(name, "/")
		bare := strings.TrimSuffix(name, "/")
		if seen[name] {
			return nil, badZip("duplicate name")
		}
		seen[name] = true
		if kind, ok := kinds[bare]; ok && kind != isDir {
			return nil, badZip("%q is both a file and a directory", bare)
		}
		kinds[bare] = isDir
		if isDir || !strings.HasPrefix(name, prefix) || len(name) <= len(prefix) {
			p = next // directory entry or outside the prefix: skipped
			continue
		}
		if flags&1 != 0 {
			return nil, badZip("encrypted entry")
		}
		if relOff+30 > cdOff {
			return nil, badZip("truncated")
		}
		lh := z[relOff : relOff+30]
		if binary.LittleEndian.Uint32(lh) != 0x04034b50 {
			return nil, badZip("bad local header")
		}
		lName := int(binary.LittleEndian.Uint16(lh[26:]))
		lExtra := int(binary.LittleEndian.Uint16(lh[28:]))
		if lName != nameLen || relOff+30+int64(lName) > cdOff || !bytes.Equal(z[relOff+30:relOff+30+int64(lName)], nameBytes) {
			return nil, badZip("name mismatch")
		}
		start := relOff + 30 + int64(lName) + int64(lExtra)
		if start+int64(csize) > cdOff {
			return nil, badZip("bad entry data")
		}
		if method != 0 && method != 8 {
			return nil, badZip("unsupported compression")
		}
		if int64(usize) > lim.MaxEntry {
			return nil, badZip("entry larger than %d bytes", lim.MaxEntry)
		}
		total += int64(usize)
		if total > lim.MaxUnpacked {
			return nil, badZip("unpacked size too large")
		}
		entries = append(entries, zipEntry{name: name, method: method, csize: csize, usize: usize, dataOff: start})
		p = next
	}
	return entries, nil
}

// unzipSave validates the whole package first (names, sizes, local headers) and only then writes it below dest, which must
// exist. Every file is created with O_EXCL (so no name can overwrite or follow an existing file or link), directories are created
// by hand with 0700, nothing but regular files is ever created, and the real decompressed size must equal the declared one.
// On any error the files already written are removed.
func unzipSave(z []byte, dest, prefix string, lim saveLimits) (err error) {
	entries, err := parseSaveZip(z, prefix, lim)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dest) // every path below is resolved inside dest: a symlink pointing out of it is refused
	if err != nil {
		return err
	}
	defer root.Close()
	var made []string
	defer func() {
		if err != nil {
			for i := len(made) - 1; i >= 0; i-- {
				_ = root.Remove(made[i])
			}
		}
	}()
	// names that are both a file and a directory ("a" and "a/b") cannot be extracted: refuse before touching the disk
	files := map[string]bool{}
	for _, e := range entries {
		files[e.name] = true
	}
	for _, e := range entries {
		for d := path.Dir(e.name); d != "." && d != "/"; d = path.Dir(d) {
			if files[d] {
				return badZip("%q is a file and a directory", d)
			}
		}
	}
	for _, e := range entries {
		// directories one component at a time (os.Root has Mkdir, not MkdirAll, in Go 1.24)
		parts := strings.Split(path.Dir(e.name), "/")
		for i := range parts {
			d := strings.Join(parts[:i+1], "/")
			if merr := root.Mkdir(d, 0o700); merr != nil && !errors.Is(merr, fs.ErrExist) {
				err = merr
				return err
			}
		}
		var f *os.File
		f, err = root.OpenFile(e.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		made = append(made, e.name)
		var r io.Reader = bytes.NewReader(z[e.dataOff : e.dataOff+int64(e.csize)])
		if e.method == 8 {
			r = flate.NewReader(r)
		}
		var n int64
		n, err = io.Copy(f, io.LimitReader(r, int64(e.usize)+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			err = badZip("corrupt entry")
			return err
		}
		if n != int64(e.usize) {
			err = badZip("declared size differs from the real size")
			return err
		}
	}
	return nil
}
