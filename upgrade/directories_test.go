// Copyright (c) 2017-2021 VMware, Inc. or its affiliates
// SPDX-License-Identifier: Apache-2.0

package upgrade_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/greenplum-db/gpupgrade/step"
	"github.com/greenplum-db/gpupgrade/testutils"
	"github.com/greenplum-db/gpupgrade/testutils/testlog"
	"github.com/greenplum-db/gpupgrade/upgrade"
	"github.com/greenplum-db/gpupgrade/utils"
	"github.com/greenplum-db/gpupgrade/utils/errorlist"
)

func TestTempDataDir(t *testing.T) {
	var id upgrade.ID

	cases := []struct {
		datadir        string
		segPrefix      string
		expectedFormat string // %s will be replaced with id.String()
	}{
		{"/data/seg-1", "seg", "/data/seg.%s.-1"},
		{"/data/master/gpseg-1", "gpseg", "/data/master/gpseg.%s.-1"},
		{"/data/seg1", "seg", "/data/seg.%s.1"},
		{"/data/seg1/", "seg", "/data/seg.%s.1"},
		{"/data/standby", "seg", "/data/standby.%s"},
	}

	for _, c := range cases {
		actual := upgrade.TempDataDir(c.datadir, c.segPrefix, id)
		expected := fmt.Sprintf(c.expectedFormat, id)

		if actual != expected {
			t.Errorf("TempDataDir(%q, %q, id) = %q, want %q",
				c.datadir, c.segPrefix, actual, expected)
		}
	}
}

func ExampleTempDataDir() {
	var id upgrade.ID

	master := upgrade.TempDataDir("/data/master/seg-1", "seg", id)
	standby := upgrade.TempDataDir("/data/standby", "seg", id)
	segment := upgrade.TempDataDir("/data/primary/seg3", "seg", id)

	fmt.Println(master)
	fmt.Println(standby)
	fmt.Println(segment)
	// Output:
	// /data/master/seg.AAAAAAAAAAA.-1
	// /data/standby.AAAAAAAAAAA
	// /data/primary/seg.AAAAAAAAAAA.3
}

func TestGetArchiveDirectoryName(t *testing.T) {
	// Make sure every part of the date is distinct, to catch mistakes in
	// formatting (e.g. using seconds rather than minutes).
	stamp := time.Date(2000, 03, 14, 12, 15, 45, 1, time.Local)

	var id upgrade.ID
	actual := upgrade.GetArchiveDirectoryName(id, stamp)

	expected := fmt.Sprintf("gpupgrade-%s-2000-03-14T12:15", id.String())
	if actual != expected {
		t.Errorf("GetArchiveDirectoryName() = %q, want %q", actual, expected)
	}
}

func TestArchiveSource(t *testing.T) {
	_, _, log := testlog.SetupLogger()

	t.Run("successfully renames source to archive, and target to source", func(t *testing.T) {
		source, target, cleanup := testutils.MustCreateDataDirs(t)
		defer cleanup(t)

		err := upgrade.ArchiveSource(source, target, true)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		testutils.VerifyRename(t, source, target)
	})

	t.Run("returns early if already renamed", func(t *testing.T) {
		source, target, cleanup := testutils.MustCreateDataDirs(t)
		defer cleanup(t)

		// To return early create archive directory
		archive := target + upgrade.OldSuffix
		err := os.Rename(target, archive)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		called := false
		utils.System.Rename = func(old, new string) error {
			called = true
			return nil
		}
		defer func() {
			utils.System.Rename = os.Rename
		}()

		testutils.VerifyRename(t, source, target)

		err = upgrade.ArchiveSource(source, target, true)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		if called {
			t.Errorf("expected rename to not be called")
		}
	})

	t.Run("bubbles up errors", func(t *testing.T) {
		source, target, cleanup := testutils.MustCreateDataDirs(t)
		defer cleanup(t)

		expected := errors.New("permission denied")
		utils.System.Rename = func(old, new string) error {
			return expected
		}
		defer func() {
			utils.System.Rename = os.Rename
		}()

		err := upgrade.ArchiveSource(source, target, true)
		if !errors.Is(err, expected) {
			t.Errorf("got %#v want %#v", err, expected)
		}
	})

	t.Run("errors when renaming a directory that is not like postgres", func(t *testing.T) {
		source := testutils.GetTempDir(t, "source")
		defer testutils.MustRemoveAll(t, source)

		target := testutils.GetTempDir(t, "target")
		defer testutils.MustRemoveAll(t, target)

		err := upgrade.ArchiveSource(source, target, true)

		var errs errorlist.Errors
		if !errors.As(err, &errs) {
			t.Fatalf("returned %#v want error type %T", err, errs)
		}

		for _, err := range errs {
			expected := upgrade.ErrInvalidDataDirectory
			if !errors.Is(err, expected) {
				t.Errorf("returned error %#v want %#v", err, expected)
			}
		}
	})

	t.Run("only renames source to archive when renameTarget is false", func(t *testing.T) {
		source, target, cleanup := testutils.MustCreateDataDirs(t)
		defer cleanup(t)

		archive := target + upgrade.OldSuffix

		calls := 0
		utils.System.Rename = func(old, new string) error {
			calls++

			if old != source {
				t.Errorf("got %q want %q", old, source)
			}

			if new != archive {
				t.Errorf("got %q want %q", new, archive)
			}

			return os.Rename(old, new)
		}
		defer func() {
			utils.System.Rename = os.Rename
		}()

		err := upgrade.ArchiveSource(source, target, false)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		if calls != 1 {
			t.Errorf("expected rename to be called once")
		}

		if upgrade.PathExists(source) {
			t.Errorf("expected source %q to not exist", source)
		}

		if !upgrade.PathExists(archive) {
			t.Errorf("expected archive %q to exist", archive)
		}
	})

	t.Run("when renaming succeeds then a re-run succeeds", func(t *testing.T) {
		source, target, cleanup := testutils.MustCreateDataDirs(t)
		defer cleanup(t)

		err := upgrade.ArchiveSource(source, target, true)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		testutils.VerifyRename(t, source, target)

		err = upgrade.ArchiveSource(source, target, true)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		testutils.VerifyRename(t, source, target)

		testlog.VerifyLogDoesNotContain(t, log, "Source directory does not exist")
	})

	t.Run("when renaming the source fails then a re-run succeeds", func(t *testing.T) {
		source, target, cleanup := testutils.MustCreateDataDirs(t)
		defer cleanup(t)

		expected := errors.New("permission denied")
		utils.System.Rename = func(old, new string) error {
			if old == source {
				return expected
			}
			return os.Rename(old, new)
		}

		err := upgrade.ArchiveSource(source, target, true)
		if !errors.Is(err, expected) {
			t.Errorf("got %#v want %#v", err, expected)
		}

		if !upgrade.PathExists(source) {
			t.Errorf("expected source %q to exist", source)
		}

		archive := target + upgrade.OldSuffix
		if upgrade.PathExists(archive) {
			t.Errorf("expected archive %q to not exist", archive)
		}

		if !upgrade.PathExists(target) {
			t.Errorf("expected target %q to exist", target)
		}

		utils.System.Rename = os.Rename

		err = upgrade.ArchiveSource(source, target, true)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		testutils.VerifyRename(t, source, target)

		testlog.VerifyLogDoesNotContain(t, log, "Source directory does not exist")
	})

	t.Run("when renaming the target fails then a re-run succeeds", func(t *testing.T) {
		source, target, cleanup := testutils.MustCreateDataDirs(t)
		defer cleanup(t)

		expected := errors.New("permission denied")
		utils.System.Rename = func(old, new string) error {
			if old == target {
				return expected
			}
			return os.Rename(old, new)
		}

		err := upgrade.ArchiveSource(source, target, true)
		if !errors.Is(err, expected) {
			t.Errorf("got %#v want %#v", err, expected)
		}

		if upgrade.PathExists(source) {
			t.Errorf("expected source %q to not exist", source)
		}

		archive := target + upgrade.OldSuffix
		if !upgrade.PathExists(archive) {
			t.Errorf("expected archive %q to exist", archive)
		}

		if !upgrade.PathExists(target) {
			t.Errorf("expected target %q to exist", target)
		}

		utils.System.Rename = os.Rename

		err = upgrade.ArchiveSource(source, target, true)
		if err != nil {
			t.Errorf("unexpected error: %#v", err)
		}

		testutils.VerifyRename(t, source, target)

		testlog.VerifyLogContains(t, log, "Source directory not found")
	})
}

func setup(t *testing.T) (teardown func(), directories []string, requiredPaths []string) {
	requiredPaths = []string{"pg_file1", "pg_file2"}
	var dataDirectories = []string{"/data/dbfast_mirror1/seg1", "/data/dbfast_mirror2/seg2"}
	rootDir, directories := setupDirs(t, dataDirectories, requiredPaths)
	teardown = func() {
		err := os.RemoveAll(rootDir)
		if err != nil {
			t.Fatalf("error %#v when deleting directory %#v", err, rootDir)
		}
	}

	return teardown, directories, requiredPaths
}

func TestDeleteDirectories(t *testing.T) {
	testlog.SetupLogger()

	utils.System.Hostname = func() (string, error) {
		return "localhost.local", nil
	}
	defer func() {
		utils.System.Hostname = os.Hostname
	}()

	t.Run("successfully deletes the directories if all required paths exist in that directory", func(t *testing.T) {
		var buf bytes.Buffer
		devNull := testutils.DevNullSpy{
			OutStream: &buf,
		}
		teardown, directories, requiredPaths := setup(t)
		defer teardown()

		err := upgrade.DeleteDirectories(directories, requiredPaths, devNull)

		if err != nil {
			t.Errorf("unexpected error got %+v", err)
		}

		for _, dataDir := range directories {
			if _, err := os.Stat(dataDir); err == nil {
				t.Errorf("dataDir %s exists", dataDir)
			}
		}

		expected := regexp.MustCompile(`Deleting directory: ".*/data/dbfast_mirror1/seg1" on host "localhost.local"\nDeleting directory: ".*/data/dbfast_mirror2/seg2" on host "localhost.local"`)

		actual := buf.String()
		if !expected.MatchString(actual) {
			t.Errorf("got stream output %s want %s", actual, expected)
		}
	})

	t.Run("rerun after a previous successfully execution must succeed", func(t *testing.T) {
		teardown, directories, requiredPaths := setup(t)
		defer teardown()

		err := upgrade.DeleteDirectories(directories, requiredPaths, step.DevNullStream)

		if err != nil {
			t.Errorf("unexpected error got %+v", err)
		}

		for _, dataDir := range directories {
			if _, err := os.Stat(dataDir); err == nil {
				t.Errorf("dataDir %s exists", dataDir)
			}
		}

		err = upgrade.DeleteDirectories(directories, requiredPaths, step.DevNullStream)

		if err != nil {
			t.Errorf("unexpected error during rerun, got %+v", err)
		}
	})

	t.Run("fails when the required paths are not in the directories", func(t *testing.T) {
		teardown, directories, _ := setup(t)
		defer teardown()

		err := upgrade.DeleteDirectories(directories, []string{"a", "b"}, step.DevNullStream)

		var errs errorlist.Errors
		if !errors.As(err, &errs) {
			t.Fatalf("got error %#v, want type %T", err, errs)
		}

		if len(errs) != 4 {
			t.Errorf("received %d errors, want %d", len(errs), 4)
		}

		for _, err := range errs {
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("got error %#v, want %#v", err, os.ErrNotExist)
			}
		}
	})

	t.Run("fails to remove one segment data directory", func(t *testing.T) {
		teardown, directories, requiredPaths := setup(t)
		defer teardown()

		fileToRemove := filepath.Join(directories[0], requiredPaths[0])
		if err := os.Remove(fileToRemove); err != nil {
			t.Errorf("unexpected error %+v", err)
		}

		err := upgrade.DeleteDirectories(directories, requiredPaths, step.DevNullStream)

		var actualErr *os.PathError
		if !errors.As(err, &actualErr) {
			t.Errorf("got error %#v, want %#v", err, "PathError")
		}

		if _, err := os.Stat(directories[0]); err != nil {
			t.Errorf("dataDir should exist, stat error %+v", err)
		}

		if _, err := os.Stat(directories[1]); err == nil {
			t.Errorf("dataDir %s exists", directories[1])
		}
	})

	t.Run("errors when hostname fails", func(t *testing.T) {
		teardown, directories, requiredPaths := setup(t)
		defer teardown()

		expected := errors.New("unable to resolve host name")
		utils.System.Hostname = func() (string, error) {
			return "", expected
		}
		defer func() {
			utils.System.Hostname = os.Hostname
		}()

		err := upgrade.DeleteDirectories(directories, requiredPaths, step.DevNullStream)
		if !errors.Is(err, expected) {
			t.Errorf("got error %#v want %#v", err, expected)
		}
	})
}

func TestTablespacePath(t *testing.T) {
	t.Run("returns correct path", func(t *testing.T) {
		path := upgrade.TablespacePath("/tmp/testfs/master/demoDataDir-1/16386", 1, 6, "301908232")
		expected := "/tmp/testfs/master/demoDataDir-1/16386/1/GPDB_6_301908232"
		if path != expected {
			t.Errorf("got %q want %q", path, expected)
		}
	})
}

func TestPathExist(t *testing.T) {
	t.Run("path exists", func(t *testing.T) {
		dir := testutils.GetTempDir(t, "")
		defer testutils.MustRemoveAll(t, dir)

		doesExist, err := upgrade.PathExist(dir)
		if err != nil {
			t.Errorf("unexpected error %#v", err)
		}

		if !doesExist {
			t.Errorf("expected path %q to exist", dir)
		}
	})

	t.Run("path does not exists", func(t *testing.T) {
		dir := testutils.GetTempDir(t, "")
		defer testutils.MustRemoveAll(t, dir)

		path := filepath.Join(dir, "doesnotexist")
		doesExist, err := upgrade.PathExist(path)
		if err != nil {
			t.Errorf("unexpected error %#v", err)
		}

		if doesExist {
			t.Errorf("expected path %q to not exist", dir)
		}
	})

	t.Run("returns error", func(t *testing.T) {
		expected := os.ErrInvalid
		utils.System.Stat = func(name string) (os.FileInfo, error) {
			return nil, expected
		}
		defer func() {
			utils.System = utils.InitializeSystemFunctions()
		}()

		doesExist, err := upgrade.PathExist("somepath")
		if !errors.Is(err, expected) {
			t.Errorf("got error %#v want %#v", err, expected)
		}

		if doesExist {
			t.Error("expected path to not exist")
		}
	})
}

// The default tablespace permissions with execute set to allow access to children
// directories and files.
const userRWX = 0700

func TestDeleteNewTablespaceDirectories(t *testing.T) {
	testlog.SetupLogger()
	utils.System.Hostname = func() (s string, err error) {
		return "", nil
	}
	defer func() {
		utils.System.Hostname = os.Hostname
	}()

	t.Run("deletes parent dbID directory when it's empty", func(t *testing.T) {
		tablespaceDir, dbIDDir, tsLocation := testutils.MustMakeTablespaceDir(t, 0)
		defer testutils.MustRemoveAll(t, tsLocation)

		err := upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, []string{tablespaceDir})
		if err != nil {
			t.Errorf("DeleteNewTablespaceDirectories returned error %+v", err)
		}

		if upgrade.PathExists(tablespaceDir) {
			t.Errorf("expected directory %q to be deleted", tablespaceDir)
		}

		if upgrade.PathExists(dbIDDir) {
			t.Errorf("expected parent dbID directory %q to be deleted", dbIDDir)
		}
	})

	t.Run("rerun of DeleteNewTablespaceDirectories after previous successful execution succeeds", func(t *testing.T) {
		tablespaceDir, dbIdDir, tsLocation := testutils.MustMakeTablespaceDir(t, 0)
		defer testutils.MustRemoveAll(t, tsLocation)

		err := upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, []string{tablespaceDir})
		if err != nil {
			t.Errorf("DeleteNewTablespaceDirectories returned error %+v", err)
		}

		if upgrade.PathExists(tablespaceDir) {
			t.Errorf("expected directory %q to be deleted", tablespaceDir)
		}

		if upgrade.PathExists(dbIdDir) {
			t.Errorf("expected parent dbid directory %q to be deleted", dbIdDir)
		}

		err = upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, []string{tablespaceDir})
		if err != nil {
			t.Errorf("rerun of DeleteNewTablespaceDirectories returned error %+v", err)
		}
	})

	t.Run("does not delete parent dbID directory when it's not empty", func(t *testing.T) {
		tablespaceDir, dbIDDir, tsLocation := testutils.MustMakeTablespaceDir(t, 0)
		defer testutils.MustRemoveAll(t, tsLocation)

		relfileNode := filepath.Join(dbIDDir, "16389")
		testutils.MustWriteToFile(t, relfileNode, "")

		err := upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, []string{tablespaceDir})
		if err != nil {
			t.Errorf("DeleteNewTablespaceDirectories returned error %+v", err)
		}

		if upgrade.PathExists(tablespaceDir) {
			t.Errorf("expected directory %q to be deleted", tablespaceDir)
		}

		if !upgrade.PathExists(dbIDDir) {
			t.Errorf("expected parent dbID directory %q to not be deleted", dbIDDir)
		}
	})

	t.Run("deletes multiple tablespace directories including their parent dbID directory when empty", func(t *testing.T) {
		type TablespaceDirs struct {
			tablespaceDir string
			dbIDDir       string
		}
		var dirs []TablespaceDirs
		var tsDirs []string

		tablespaceOids := []int{16386, 16387, 16388}
		for _, oid := range tablespaceOids {
			tablespaceDir, dbIdDir, tsLocation := testutils.MustMakeTablespaceDir(t, oid)
			defer testutils.MustRemoveAll(t, tsLocation)

			dirs = append(dirs, TablespaceDirs{tablespaceDir, dbIdDir})
			tsDirs = append(tsDirs, tablespaceDir)
		}

		err := upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, tsDirs)
		if err != nil {
			t.Errorf("DeleteNewTablespaceDirectories returned error %+v", err)
		}

		for _, dir := range dirs {
			if upgrade.PathExists(dir.tablespaceDir) {
				t.Errorf("expected directory %q to be deleted", dir.tablespaceDir)
			}

			if upgrade.PathExists(dir.dbIDDir) {
				t.Errorf("expected parent dbID directory %q to be deleted", dir.dbIDDir)
			}
		}
	})

	t.Run("errors when tablespace directory is invalid", func(t *testing.T) {
		tablespaceDir, _, tsLocation := testutils.MustMakeTablespaceDir(t, 0)
		defer testutils.MustRemoveAll(t, tsLocation)

		invalidTablespaceDir := testutils.GetTempDir(t, "invalidTablespace")
		defer testutils.MustRemoveAll(t, invalidTablespaceDir)

		dirs := []string{tablespaceDir, invalidTablespaceDir}
		err := upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, dirs)
		if !errors.Is(err, upgrade.ErrInvalidTablespaceDirectory) {
			t.Errorf("got error %#v want %#v", err, upgrade.ErrInvalidTablespaceDirectory)
		}

		for _, dir := range dirs {
			if !upgrade.PathExists(dir) {
				t.Errorf("expected directory %q to not be deleted", dir)
			}

			dbIdDir := filepath.Dir(filepath.Clean(dir))
			if !upgrade.PathExists(dbIdDir) {
				t.Errorf("expected parent dbID directory %q to not be deleted", dbIdDir)
			}
		}
	})

	t.Run("errors when tablespace directory can't be deleted", func(t *testing.T) {
		tablespaceDir, dbIDDir, tsLocation := testutils.MustMakeTablespaceDir(t, 0)
		defer func() {
			err := os.Chmod(dbIDDir, userRWX)
			if err != nil {
				t.Fatalf("making parent dbId directory writeable: %v", err)
			}
			testutils.MustRemoveAll(t, tsLocation)
		}()

		// Set parent dbID directory to read only so its children cannot be
		// removed.
		err := os.Chmod(dbIDDir, 0500)
		if err != nil {
			t.Fatalf("making parent dbID directory read only: %v", err)
		}

		err = upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, []string{tablespaceDir})

		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("got error %#v want %#v", err, os.ErrPermission)
		}

		if !upgrade.PathExists(tablespaceDir) {
			t.Errorf("expected directory %q to not be deleted", tablespaceDir)
		}

		if !upgrade.PathExists(dbIDDir) {
			t.Errorf("expected parent dbID directory %q to not be deleted", dbIDDir)
		}
	})

	t.Run("errors when failing to read parent dbID directory", func(t *testing.T) {
		tablespaceDir, dbIDDir, tsLocation := testutils.MustMakeTablespaceDir(t, 0)
		defer func() {
			err := os.Chmod(dbIDDir, userRWX)
			if err != nil {
				t.Fatalf("making parent dbID directory writeable: %v", err)
			}
			testutils.MustRemoveAll(t, tsLocation)
		}()

		// Set parent dbid directory to write and execute to allow its children
		// to be removed, but does not allow its contents to be read.
		err := os.Chmod(dbIDDir, 0300)
		if err != nil {
			t.Fatalf("making parent directory read only: %v", err)
		}

		err = upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, []string{tablespaceDir})
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("got error %#v want %#v", err, os.ErrPermission)
		}

		if upgrade.PathExists(tablespaceDir) {
			t.Errorf("expected directory %q to be deleted", tablespaceDir)
		}

		if !upgrade.PathExists(dbIDDir) {
			t.Errorf("expected parent dbID directory %q to not be deleted", dbIDDir)
		}
	})

	t.Run("errors when failing to remove parent dbID directory", func(t *testing.T) {
		tablespaceDir, dbIDDir, tsLocation := testutils.MustMakeTablespaceDir(t, 0)
		defer func() {
			err := os.Chmod(tsLocation, userRWX)
			if err != nil {
				t.Fatalf("making tablespace location writeable: %v", err)
			}
			testutils.MustRemoveAll(t, tsLocation)
		}()

		// Set tablespace location to read and execute to allow its children
		// to be removed.
		err := os.Chmod(tsLocation, 0500)
		if err != nil {
			t.Fatalf("making tablespace location directory read only: %v", err)
		}

		err = upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, []string{tablespaceDir})
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("got error %#v want %#v", err, os.ErrPermission)
		}

		if upgrade.PathExists(tablespaceDir) {
			t.Errorf("expected directory %q to be deleted", tablespaceDir)
		}

		if !upgrade.PathExists(dbIDDir) {
			t.Errorf("expected parent dbid directory %q to not be deleted", dbIDDir)
		}
	})

	t.Run("rerun finishes successfully", func(t *testing.T) {
		type TablespaceDirs struct {
			tablespaceDir string
			dbIdDir       string
		}
		var dirs []TablespaceDirs
		var tsDirs []string

		tablespaceOids := []int{16386, 16387, 16388}
		for _, oid := range tablespaceOids {
			tablespaceDir, dbIdDir, tsLocation := testutils.MustMakeTablespaceDir(t, oid)
			defer testutils.MustRemoveAll(t, tsLocation)

			dirs = append(dirs, TablespaceDirs{tablespaceDir, dbIdDir})
			tsDirs = append(tsDirs, tablespaceDir)
		}

		err := upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, tsDirs)
		if err != nil {
			t.Errorf("DeleteNewTablespaceDirectories returned error %+v", err)
		}

		for _, dir := range dirs {
			if upgrade.PathExists(dir.tablespaceDir) {
				t.Errorf("expected directory %q to be deleted", dir.tablespaceDir)
			}

			if upgrade.PathExists(dir.dbIdDir) {
				t.Errorf("expected parent dbid directory %q to be deleted", dir.dbIdDir)
			}
		}

		err = upgrade.DeleteNewTablespaceDirectories(step.DevNullStream, tsDirs)
		if err != nil {
			t.Errorf("unexpected error %+v", err)
		}
	})
}

func TestVerify5XTablespaceDirectories(t *testing.T) {
	t.Run("succeeds when given multiple 5X tablespace locations", func(t *testing.T) {
		var dirs []string
		tablespaceOids := []int{16386, 16387, 16388}
		for _, oid := range tablespaceOids {
			_, tsLocationDir := testutils.MustMake5XTablespaceDir(t, oid)
			defer testutils.MustRemoveAll(t, tsLocationDir)

			dirs = append(dirs, tsLocationDir)
		}

		err := upgrade.Verify5XTablespaceDirectories(dirs)
		if err != nil {
			t.Errorf("Verify5XTablespaceDirectories returned error %+v", err)
		}
	})

	t.Run("succeeds when tablespace directory contains other files but no dbOID directory", func(t *testing.T) {
		tsLocationDir := testutils.GetTempDir(t, "")
		defer testutils.MustRemoveAll(t, tsLocationDir)

		testutils.MustWriteToFile(t, filepath.Join(tsLocationDir, "foo"), "")

		err := upgrade.Verify5XTablespaceDirectories([]string{tsLocationDir})
		if err != nil {
			t.Errorf("Verify5XTablespaceDirectories returned error %+v", err)
		}
	})

	t.Run("errors when failing to read tablespace directory", func(t *testing.T) {
		tsLocationDir := testutils.GetTempDir(t, "")
		defer func() {
			err := os.Chmod(tsLocationDir, userRWX)
			if err != nil {
				t.Fatalf("making tablespace location directory writeable: %v", err)
			}
			testutils.MustRemoveAll(t, tsLocationDir)
		}()

		// Set tablespace directory to write and execute to prevent its contents
		// from being read.
		err := os.Chmod(tsLocationDir, 0300)
		if err != nil {
			t.Fatalf("making tablespace directory non-readable: %v", err)
		}

		err = upgrade.Verify5XTablespaceDirectories([]string{tsLocationDir})
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("got error %#v want %#v", err, os.ErrPermission)
		}
	})

	t.Run("errors when dbOID directory does not contain required file", func(t *testing.T) {
		dbOIDDir, tsLocationDir := testutils.MustMake5XTablespaceDir(t, 0)
		defer testutils.MustRemoveAll(t, tsLocationDir)

		err := os.Remove(filepath.Join(dbOIDDir, upgrade.PGVersion))
		if err != nil {
			t.Fatalf("removing PG_VERSION from %q: %v", dbOIDDir, err)
		}

		err = upgrade.Verify5XTablespaceDirectories([]string{tsLocationDir})

		if !errors.Is(err, upgrade.ErrInvalidTablespaceDirectory) {
			t.Errorf("got error %#v want %#v", err, upgrade.ErrInvalidTablespaceDirectory)
		}
	})
}

func setupDirs(t *testing.T, subdirectories []string, requiredPaths []string) (tmpDir string, createdDirectories []string) {
	var err error
	tmpDir, err = ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temporary directory: %v", err)
	}

	for _, dir := range subdirectories {
		createdDirectories = append(createdDirectories, createDataDir(t, dir, tmpDir, requiredPaths))
	}

	return tmpDir, createdDirectories
}

func createDataDir(t *testing.T, name, tmpDir string, requiredPaths []string) (dirPath string) {
	dirPath = filepath.Join(tmpDir, name)

	err := os.MkdirAll(dirPath, userRWX)
	if err != nil {
		t.Errorf("error creating path: %v", err)
	}

	for _, fileName := range requiredPaths {
		filePath := filepath.Join(dirPath, fileName)
		testutils.MustWriteToFile(t, filePath, "")
	}

	return dirPath
}
