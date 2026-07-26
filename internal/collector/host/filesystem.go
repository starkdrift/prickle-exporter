// SPDX-License-Identifier: Apache-2.0

package host

import (
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// FSStats is the subset of statfs(2) the exporter reports.
//
// Fields are kept in the syscall's own units — blocks, plus the block size —
// rather than pre-multiplied, so a fake in a test can carry the exact numbers
// `df -B1` printed on the captured host without reversing the arithmetic.
type FSStats struct {
	BlockSize   int64  // f_bsize
	Blocks      uint64 // f_blocks: total data blocks
	BlocksFree  uint64 // f_bfree: free blocks, including the root reserve
	BlocksAvail uint64 // f_bavail: free blocks available to unprivileged users
	Files       uint64 // f_files: total inodes
	FilesFree   uint64 // f_ffree: free inodes
}

// Statfser reports filesystem capacity for a mount point.
//
// SPEC.md §Collectors makes this an interface because statfs is a syscall, not
// a file: it is the one Phase 1 source that cannot be captured into a fixture
// tree. Tests supply a fake seeded from the capture's meta/statfs-reference.txt.
type Statfser interface {
	Statfs(path string) (FSStats, error)
}

// mount is one line of /proc/mounts.
type mount struct {
	device     string
	mountPoint string
	fsType     string
	readOnly   bool
}

// collectFilesystem parses /proc/mounts and calls Statfs for each mount that
// survives the exclusion filters.
//
// The hot series are keyed by mountpoint alone. Device and filesystem type are
// descriptive, so they live on the _info gauge (SPEC.md §Metrics contract):
// mountpoint is what an alert is written against, and it is already unique.
func (c *Collector) collectFilesystem(out *exposition.Set) error {
	mounts, err := c.readMounts()
	if err != nil {
		return err
	}

	size := out.Gauge(prefix+"filesystem_size_bytes", "Filesystem capacity.")
	free := out.Gauge(prefix+"filesystem_free_bytes", "Filesystem space free, including the root reserve.")
	avail := out.Gauge(prefix+"filesystem_avail_bytes", "Filesystem space available to unprivileged users.")
	files := out.Gauge(prefix+"filesystem_files", "Filesystem total inodes.")
	filesFree := out.Gauge(prefix+"filesystem_files_free", "Filesystem free inodes.")
	readOnly := out.Gauge(prefix+"filesystem_readonly", "Whether the filesystem is mounted read-only.")
	errored := out.Gauge(prefix+"filesystem_error", "Whether the last statfs of this filesystem failed.")
	info := out.Gauge(prefix+"filesystem_info",
		"Filesystem identity: constant 1, carrying the backing device and filesystem type.")

	seen := make(map[string]struct{}, len(mounts))
	for _, m := range mounts {
		if c.opts.ExcludedFSTypes != nil && c.opts.ExcludedFSTypes.MatchString(m.fsType) {
			continue
		}
		if c.opts.ExcludedMountPoints != nil && c.opts.ExcludedMountPoints.MatchString(m.mountPoint) {
			continue
		}
		if _, dup := seen[m.mountPoint]; dup {
			// A second mount on the same point shadows the first. Only the
			// visible one has meaningful numbers, and a repeated series would
			// cost the whole scrape.
			continue
		}
		seen[m.mountPoint] = struct{}{}

		mp := exposition.L("mountpoint", m.mountPoint)
		info.Add(1, mp, exposition.L("device", m.device), exposition.L("fstype", m.fsType))
		readOnly.Add(boolValue(m.readOnly), mp)

		st, err := c.opts.Statfs.Statfs(m.mountPoint)
		if err != nil {
			// Expected in normal operation: an autofs point that would block,
			// or a network mount whose server is gone. Report it as a series
			// an operator can alert on rather than as a collector failure.
			errored.Add(1, mp)
			continue
		}
		errored.Add(0, mp)

		bs := float64(st.BlockSize)
		size.Add(float64(st.Blocks)*bs, mp)
		free.Add(float64(st.BlocksFree)*bs, mp)
		avail.Add(float64(st.BlocksAvail)*bs, mp)
		files.Add(float64(st.Files), mp)
		filesFree.Add(float64(st.FilesFree), mp)
	}
	return nil
}

// readMounts parses /proc/mounts.
//
//	/dev/vda1 / ext4 rw,relatime,discard,errors=remount-ro 0 0
//
// Six space-separated fields; the first four are used. Lines with fewer are
// skipped rather than treated as errors — /proc/mounts is written by the kernel
// one mount at a time and a torn read is possible in principle.
func (c *Collector) readMounts() ([]mount, error) {
	path := c.opts.Roots.ProcPath("mounts")
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}

	mounts := make([]mount, 0, len(lines))
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		mounts = append(mounts, mount{
			device:     unescapeMountField(f[0]),
			mountPoint: unescapeMountField(f[1]),
			fsType:     f[2],
			readOnly:   hasMountOption(f[3], "ro"),
		})
	}
	return mounts, nil
}

// hasMountOption reports whether a comma-separated option list contains opt.
// Matching the whole element matters: "ro" must not match "rootcontext=...".
func hasMountOption(opts, opt string) bool {
	for len(opts) > 0 {
		var head string
		head, opts, _ = strings.Cut(opts, ",")
		if head == opt {
			return true
		}
	}
	return false
}

// unescapeMountField decodes the octal escapes the kernel writes into
// /proc/mounts for characters that would otherwise break the field split:
// space (\040), tab (\011), newline (\012) and backslash (\134).
func unescapeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v := (s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0')
			b.WriteByte(v)
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
