// SPDX-License-Identifier: Apache-2.0

package host

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// syntheticInodes supplies the one thing the capture could not.
//
// SPEC.md §Collectors puts statfs behind an interface because it is a syscall,
// so scripts/capture-fixtures.sh writes `df -B1` output instead — and df does
// not report inode counts. These numbers are HAND-BUILT, not captured, and
// exist only to exercise the filesystem_files code path. Everything else in
// fakeStatfs comes from testdata/h200-ubuntu2204/statfs-reference.txt.
//
// /boot/efi is 0/0 because vfat genuinely has no inode table; that is the one
// entry here that is realistic rather than merely plausible.
var syntheticInodes = map[string][2]uint64{
	"/":           {46874624, 46242113},
	"/run":        {30909181, 30908277},
	"/run/lock":   {30909181, 30909178},
	"/boot/efi":   {0, 0},
	"/run/user/0": {6185143, 6185139},
}

// fakeStatfs answers from the capture's df output.
type fakeStatfs struct {
	byMountPoint map[string]FSStats
}

func newFakeStatfs(t *testing.T) *fakeStatfs {
	t.Helper()
	return newFakeStatfsFrom(t, filepath.Join(fixtureDir, "statfs-reference.txt"))
}

// newFakeStatfsFrom parses the `df -B1` table:
//
//	Mounted on   Type   1B-blocks   Used   Avail
//	/            ext4   749062754304 18602811392 730443165696
//
// Block size is reported as 1 so the block counts are already bytes; free is
// derived as total minus used, which includes the root reserve that Avail
// excludes — the same relationship statfs's f_bfree and f_bavail have.
func newFakeStatfsFrom(t *testing.T, path string) *fakeStatfs {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeStatfs{byMountPoint: make(map[string]FSStats)}
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if i == 0 {
			continue // header
		}
		cols := strings.Fields(line)
		if len(cols) != 5 {
			t.Fatalf("%s:%d: expected 5 columns, got %d: %q", path, i+1, len(cols), line)
		}
		total, used, avail := mustUint(t, cols[2]), mustUint(t, cols[3]), mustUint(t, cols[4])
		inodes := syntheticInodes[cols[0]]
		f.byMountPoint[cols[0]] = FSStats{
			BlockSize:   1,
			Blocks:      total,
			BlocksFree:  total - used,
			BlocksAvail: avail,
			Files:       inodes[0],
			FilesFree:   inodes[1],
		}
	}
	return f
}

func (f *fakeStatfs) Statfs(path string) (FSStats, error) {
	st, ok := f.byMountPoint[path]
	if !ok {
		return FSStats{}, os.ErrNotExist
	}
	return st, nil
}

func mustUint(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestFilesystemExclusions checks the defaults against the captured mount
// table: 60 mounts in, five out. This is the test that would catch a default
// regexp change quietly restoring per-container overlay series.
func TestFilesystemExclusions(t *testing.T) {
	out := collectFixture(t)

	got := seriesLabels(out, "prickle_host_filesystem_size_bytes")
	want := []string{
		`{node="fixture",mountpoint="/run"}`,
		`{node="fixture",mountpoint="/"}`,
		`{node="fixture",mountpoint="/run/lock"}`,
		`{node="fixture",mountpoint="/boot/efi"}`,
		`{node="fixture",mountpoint="/run/user/0"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("filesystems reported:\n got %v\nwant %v", got, want)
	}
}

// TestFilesystemInfoCarriesNames checks SPEC.md §Metrics contract: the hot
// series carry only the mountpoint key, and the descriptive device and fstype
// ride on the _info gauge.
func TestFilesystemInfoCarriesNames(t *testing.T) {
	out := collectFixture(t)

	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "prickle_host_filesystem_") ||
			strings.HasPrefix(line, "prickle_host_filesystem_info") {
			continue
		}
		for _, forbidden := range []string{"device=", "fstype="} {
			if strings.Contains(line, forbidden) {
				t.Errorf("descriptive label %q on a hot series: %s", forbidden, line)
			}
		}
	}

	want := `prickle_host_filesystem_info{node="fixture",mountpoint="/",device="/dev/vda1",fstype="ext4"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing info series:\n%s", want)
	}
}

// TestStatfsFailureIsASeries checks that an unreadable mount is reported as
// filesystem_error rather than failing the collection: a wedged NFS server
// must not cost the host its CPU metrics.
func TestStatfsFailureIsASeries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc", "mounts"),
		"/dev/vda1 / ext4 rw,relatime 0 0\n"+
			"nfs-server:/export /mnt/wedged nfs4 rw,relatime 0 0\n")

	set := newSet()
	// The fake knows about "/" and nothing else, so /mnt/wedged fails.
	c := New(Options{Roots: fsroot.At(dir), Statfs: newFakeStatfs(t)})
	if err := c.collectFilesystem(set); err != nil {
		t.Fatalf("collectFilesystem returned an error for one bad mount: %v", err)
	}

	out := set.String()
	if !strings.Contains(out, `prickle_host_filesystem_error{node="test",mountpoint="/mnt/wedged"} 1`) {
		t.Errorf("wedged mount not reported as an error series:\n%s", out)
	}
	if !strings.Contains(out, `prickle_host_filesystem_error{node="test",mountpoint="/"} 0`) {
		t.Errorf("healthy mount not reported as error 0:\n%s", out)
	}
	if strings.Contains(out, `prickle_host_filesystem_size_bytes{node="test",mountpoint="/mnt/wedged"}`) {
		t.Error("wedged mount produced capacity series")
	}
}

// TestReadOnlyMounts checks the ro option is detected as a whole option and
// not as a prefix of another.
func TestReadOnlyMounts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc", "mounts"),
		"/dev/vda1 / ext4 rw,relatime 0 0\n"+
			"/dev/vdb /ro ext4 ro,relatime 0 0\n"+
			"/dev/vdc /tricky ext4 rw,rootcontext=system_u:object_r:root_t 0 0\n")

	set := newSet()
	c := New(Options{Roots: fsroot.At(dir), Statfs: &fakeStatfs{}})
	if err := c.collectFilesystem(set); err != nil {
		t.Fatal(err)
	}

	out := set.String()
	for _, want := range []string{
		`prickle_host_filesystem_readonly{node="test",mountpoint="/"} 0`,
		`prickle_host_filesystem_readonly{node="test",mountpoint="/ro"} 1`,
		`prickle_host_filesystem_readonly{node="test",mountpoint="/tricky"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing:\n%s\ngot:\n%s", want, out)
		}
	}
}

// TestUnescapeMountField covers the octal escapes the kernel writes for
// characters that would otherwise break the field split.
func TestUnescapeMountField(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/", "/"},
		{"/mnt/plain", "/mnt/plain"},
		{`/mnt/with\040space`, "/mnt/with space"},
		{`/mnt/tab\011here`, "/mnt/tab\there"},
		{`/mnt/back\134slash`, `/mnt/back\slash`},
		{`/mnt/new\012line`, "/mnt/new\nline"},
		{`/mnt/\040\040two`, "/mnt/  two"},
		// A backslash that is not a complete octal escape is literal.
		{`/mnt/lone\x`, `/mnt/lone\x`},
		{`/mnt/short\04`, `/mnt/short\04`},
	} {
		if got := unescapeMountField(tc.in); got != tc.want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDuplicateMountPointIsDropped checks that a shadowed mount does not
// produce a duplicate series — Prometheus rejects a whole scrape over one.
func TestDuplicateMountPointIsDropped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc", "mounts"),
		"/dev/vda1 / ext4 rw,relatime 0 0\n"+
			"/dev/vdb / ext4 rw,relatime 0 0\n")

	set := newSet()
	c := New(Options{Roots: fsroot.At(dir), Statfs: newFakeStatfs(t)})
	if err := c.collectFilesystem(set); err != nil {
		t.Fatal(err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("duplicate series reached the renderer: %v", err)
	}
	if n := countMatching(set.String(), "prickle_host_filesystem_info{"); n != 1 {
		t.Errorf("info series = %d, want 1", n)
	}
}

// seriesLabels returns the label sets of every sample of one metric, in output
// order.
func seriesLabels(rendered, metric string) []string {
	var out []string
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		labels, _, _ := strings.Cut(strings.TrimPrefix(line, metric), " ")
		out = append(out, labels)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
