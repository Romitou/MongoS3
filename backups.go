package main

import (
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// backupSet groups the objects of one backup run: the archive and its logs.
type backupSet struct {
	Base    string // object name without extension, e.g. "dump.2026-01-01T000000Z"
	Archive *minio.ObjectInfo
	Objects []minio.ObjectInfo
	// Time is the most recent modification date of the objects.
	Time time.Time
}

// backupBase returns the name of an object without its extension
// (".archive", ".archive.gz.age", ".log", the legacy ".logs"...). Object
// names are "<prefix>dump.<timestamp>.<extension>", the timestamp has no dot.
func backupBase(key string) (base string, isArchive bool) {
	start := strings.LastIndex(key, backupObjectPrefix)
	if start < 0 {
		return key, false
	}
	start += len(backupObjectPrefix)
	dot := strings.IndexByte(key[start:], '.')
	if dot < 0 {
		return key, false
	}
	return key[:start+dot], strings.HasPrefix(key[start+dot:], ".archive")
}

// groupBackups groups objects by backup run, newest first.
func groupBackups(objects []minio.ObjectInfo) []*backupSet {
	byBase := map[string]*backupSet{}
	var sets []*backupSet
	for _, object := range objects {
		base, isArchive := backupBase(object.Key)
		set, ok := byBase[base]
		if !ok {
			set = &backupSet{Base: base}
			byBase[base] = set
			sets = append(sets, set)
		}
		set.Objects = append(set.Objects, object)
		if isArchive {
			archive := object
			set.Archive = &archive
		}
		if object.LastModified.After(set.Time) {
			set.Time = object.LastModified
		}
	}
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].Time.After(sets[j].Time) })
	return sets
}

// previousArchive returns the most recent archive other than the current
// backup, or nil. Only archives with the same extension are considered:
// comparing a gzipped archive with a raw one would be meaningless.
func previousArchive(sets []*backupSet, currentBase, extension string) *minio.ObjectInfo {
	for _, set := range sets {
		if set.Base != currentBase && set.Archive != nil && strings.HasSuffix(set.Archive.Key, extension) {
			return set.Archive
		}
	}
	return nil
}

// expiredBackups returns the backups older than cutoff, always keeping the
// keepMin most recent archives: if backups keep failing, the last good ones
// are never deleted.
func expiredBackups(sets []*backupSet, cutoff time.Time, keepMin int) []*backupSet {
	var expired []*backupSet
	kept := 0
	for _, set := range sets {
		if set.Archive != nil && kept < keepMin {
			kept++
			continue
		}
		if set.Time.Before(cutoff) {
			expired = append(expired, set)
		}
	}
	return expired
}

// sizeDrop returns by how many percent current is smaller than previous
// (negative when it is bigger).
func sizeDrop(previous, current int64) float64 {
	if previous <= 0 {
		return 0
	}
	return float64(previous-current) / float64(previous) * 100
}
