package testdb

import (
	"regexp"
	"strings"
	"testing"
)

func TestDBNameIsUniqueSafeAndShort(t *testing.T) {
	valid := regexp.MustCompile(`^dost_test_[a-z0-9_]*_[0-9a-f]{8}$`)
	long := "TestİstanbulŞehri/" + strings.Repeat("sub test ", 20)
	a, b := dbName(long), dbName(long)
	if a == b {
		t.Errorf("two calls returned %q", a)
	}
	for _, n := range []string{a, dbName("TestX"), dbName("")} {
		if len(n) > 63 || !valid.MatchString(n) {
			t.Errorf("dbName = %q (%d bytes), want a lower-case identifier of at most 63 bytes", n, len(n))
		}
	}
}

func TestNewSkipsWithoutServer(t *testing.T) {
	t.Setenv(EnvURL, "")
	ran := false
	t.Run("inner", func(t *testing.T) {
		New(t)
		ran = true
	})
	if ran {
		t.Error("New returned without a server; want a skip")
	}
}
