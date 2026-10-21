package gitcmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// MinVersion is the oldest git that git-svc supports. The merge engine
// needs `git merge-tree --write-tree`, which arrived in 2.38.
var MinVersion = Version{Major: 2, Minor: 38}

// Version is a git release number.
type Version struct {
	Major, Minor, Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// ParseVersion parses the output of `git version`, for example
// "git version 2.42.0.windows.2" or "git version 2.39.5 (Apple Git-154)".
func ParseVersion(out string) (Version, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(out), "git version ")
	if !ok {
		return Version{}, fmt.Errorf("unexpected git version output %q", out)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Version{}, fmt.Errorf("unexpected git version output %q", out)
	}

	// Vendor suffixes follow the numbers: "2.42.0.windows.2", "2.45.0.rc1".
	parts := strings.Split(fields[0], ".")
	nums := make([]int, 0, 3)
	for _, p := range parts {
		if len(nums) == 3 {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		nums = append(nums, n)
	}
	if len(nums) < 2 {
		return Version{}, fmt.Errorf("unexpected git version output %q", out)
	}
	v := Version{Major: nums[0], Minor: nums[1]}
	if len(nums) == 3 {
		v.Patch = nums[2]
	}
	return v, nil
}

// Version runs `git version` and parses its output.
func (r *Runner) Version(ctx context.Context) (Version, error) {
	out, err := r.Run(ctx, "", "version")
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(string(out))
}

// CheckVersion returns the installed git version, or an error if it is
// older than minVersion.
func (r *Runner) CheckVersion(ctx context.Context, minVersion Version) (Version, error) {
	v, err := r.Version(ctx)
	if err != nil {
		return Version{}, err
	}
	if v.Less(minVersion) {
		return v, fmt.Errorf("git %s at %s is too old: git-svc needs %s or newer", v, r.bin, minVersion)
	}
	return v, nil
}
