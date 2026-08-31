package buildinfo

import "testing"

func TestParseVersionAndCompare(t *testing.T) {
	for _, invalid := range []string{"v1.2.3", "server-v1.2.3", "1.2", "01.2.3", "1.2.3-beta", "18446744073709551616.0.0"} {
		if _, err := ParseVersion(invalid); err == nil {
			t.Fatalf("ParseVersion(%q) unexpectedly succeeded", invalid)
		}
	}
	one, err := ParseVersion("1.20.3")
	if err != nil {
		t.Fatal(err)
	}
	two, _ := ParseVersion("2.0.0")
	if one.String() != "1.20.3" || one.Compare(two) >= 0 || two.Compare(one) <= 0 || one.Compare(one) != 0 {
		t.Fatalf("unexpected semantic version behavior: one=%+v two=%+v", one, two)
	}
}

func TestCurrentKeepsDevelopmentBuildNonComparable(t *testing.T) {
	oldVersion, oldCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })

	Version, Commit = "dev", "unknown"
	if info := Current(); info.Version != "dev" || info.Comparable || info.Official {
		t.Fatalf("unexpected development info: %+v", info)
	}
	Version, Commit = "1.2.3", "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	if info := Current(); !info.Comparable || !info.Official || info.Commit != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("unexpected official info: %+v", info)
	}
	Version, Commit = "not-a-version", "abcdef0123456789abcdef0123456789abcdef01"
	if info := Current(); info.Version != "dev" || info.Comparable || info.Official {
		t.Fatalf("invalid linker input must fail closed: %+v", info)
	}
}
