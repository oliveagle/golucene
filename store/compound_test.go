package store

import (
	"testing"
)

func TestReadEntriesOSX(t *testing.T) {
	path := "../search/testdata/osx/belfrysample"
	d, err := OpenFSDirectory(path)
	if err != nil {
		t.Error(err)
	}
	ctx := NewIOContextBool(false)
	handle, err := d.createSlicer("_0.cfs", ctx)
	if err != nil {
		t.Error(err)
	}
	m, err := readEntries(handle, d, "_0.cfs")
	if err != nil {
		t.Error(err)
	}
	if len(m) != 9 {
		t.Errorf("Should have 9 entries.")
	}
	f := m[".fnm"]
	if f.offset != 31 || f.length != 541 {
		t.Errorf("'.fnm' (offset=31, length=541), now %v", f)
	}
	f = m["_Lucene41_0.tip"]
	if f.offset != 9820 || f.length != 252 {
		t.Errorf("'_Lucene41_0.tip' (offset=9820, length=242), now %v", f)
	}
}