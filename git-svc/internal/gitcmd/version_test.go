package gitcmd

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		out     string
		want    Version
		wantErr bool
	}{
		{out: "git version 2.38.0\n", want: Version{2, 38, 0}},
		{out: "git version 2.42.0.windows.2", want: Version{2, 42, 0}},
		{out: "git version 2.39.5 (Apple Git-154)", want: Version{2, 39, 5}},
		{out: "git version 2.45.0.rc1", want: Version{2, 45, 0}},
		{out: "git version 2.47", want: Version{2, 47, 0}},
		{out: "git version 3.0.1", want: Version{3, 0, 1}},
		{out: "", wantErr: true},
		{out: "git version", wantErr: true},
		{out: "git version two.0", wantErr: true},
		{out: "git version 2", wantErr: true},
		{out: "hub version 2.14.2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.out, func(t *testing.T) {
			got, err := ParseVersion(tt.out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseVersion(%q) error = %v, wantErr %v", tt.out, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseVersion(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestVersionLess(t *testing.T) {
	tests := []struct {
		a, b Version
		want bool
	}{
		{Version{2, 37, 9}, MinVersion, true},
		{Version{2, 38, 0}, MinVersion, false},
		{Version{2, 42, 0}, MinVersion, false},
		{Version{1, 99, 99}, MinVersion, true},
		{Version{3, 0, 0}, MinVersion, false},
		{Version{2, 38, 1}, Version{2, 38, 2}, true},
	}
	for _, tt := range tests {
		if got := tt.a.Less(tt.b); got != tt.want {
			t.Errorf("%v.Less(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
