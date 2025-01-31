package core

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Sirupsen/logrus"
)

var (
	// All tests will be saved to testTempDir instead of "/tmp". Saving test output to "/tmp" directory can cause
	// problems with testing if "/tmp" is mounted to memory. The Kernel reclaims as much space as possible, this causes
	// directory sizes to behave differently when files are removed from the directory. In a normal filesystem, the
	// directory sizes are unchanged after files are removed from the directory, but in a RAM mounted /tmp, the directory
	// sizes are reclaimed immediately.
	testTempDir = func() string {
		cdir, _ := os.Getwd()
		return path.Clean(path.Join(cdir, "..", "..", "testdata", "temp"))
	}()
)

// fileTests test subdirectory creation, fileinfo synchronization, and file duplication.
type syncTest struct {
	t   *testing.T
	ctx *Context

	outputStreams     uint16
	paddingPercentage float64
	backupPath        string
	fileIndex         func() FileIndex
	deviceList        func() DeviceList
	saveSyncContext   bool

	errors       []error // These are checked
	errChan      *chan error
	expectErrors func() []error

	expectDeviceUsage func() []expectDevice

	dumpFileIndex bool // If set to true, the FileIndex is dumped to stdout witohut syncing

	// done chan bool
}

// checkErrors checks errors returned from a test against any expected errors. Returns a bool that tells the caller the
func (s *syncTest) checkErrors() {
	if len(s.errors) == 0 && s.expectErrors != nil {
		for _, e2 := range s.expectErrors() {
			s.t.Errorf("EXPECT: Error '%s'\n\t GOT: Nil", e2)
		}
		s.t.FailNow()
	} else if len(s.errors) != 0 && s.expectErrors == nil {
		for _, e2 := range s.errors {
			s.t.Errorf("Expect: No errors\n\t Got: %+v", e2)
		}
		s.t.FailNow()
	} else if len(s.errors) != 0 {
		// Check the expected errors
		for _, e := range s.errors {
			foundErr := false
			for _, e2 := range s.expectErrors() {
				if reflect.TypeOf(e) == reflect.TypeOf(e2) {
					foundErr = true
				}
			}
			if !foundErr {
				// s.t.Errorf("EXPECT: Error TypeOf %s GOT: %#v", reflect.TypeOf(e), s.expectErrors)
				s.t.Errorf("EXPECT: Error %#v\n\t GOT: %#v", s.expectErrors(), e)
				s.t.FailNow()
			}
		}
	}
}

// checkPerms will check uid, gid, and mod time of the destination files
func (s *syncTest) checkPerms(f *File) {
	for _, df := range f.DestFiles {
		fi, err := os.Lstat(df.Path)
		if err != nil {
			s.t.Error(err)
			continue
		}
		if fi.Mode() != f.Mode {
			s.t.Errorf("File: %q\n\t Got Mode: %q Expect: %q\n", f.Name, fi.Mode(), f.Mode)
		}
		if fi.ModTime() != f.ModTime {
			s.t.Errorf("File: %q\n\t Got ModTime: %q Expect: %q\n", f.Name, fi.ModTime(), f.ModTime)
		}
		if int(fi.Sys().(*syscall.Stat_t).Uid) != f.Owner {
			s.t.Errorf("File: %q\n\t Got Owner: %q Expect: %q\n", f.ModTime,
				int(fi.Sys().(*syscall.Stat_t).Uid), f.Owner)
		}
		if int(fi.Sys().(*syscall.Stat_t).Gid) != f.Group {
			s.t.Errorf("File: %q\n\t Got Group: %d Expect: %d\n", f.Name,
				int(fi.Sys().(*syscall.Stat_t).Gid), f.Group)
		}
	}
}

// checkDestSize checks the sizes of the destination files
func (s *syncTest) checkDestSize(f *File) {
	for _, df := range f.DestFiles {
		ls, err := os.Lstat(df.Path)
		if err != nil {
			s.t.Error(err)
			continue
		}
		if df.EndByte == 0 && uint64(ls.Size()) != f.Size {
			s.t.Errorf("File: %q\n\t  Got Size: %d Expect: %d\n", df.Path, ls.Size, f.Size)
		} else if df.EndByte != 0 && uint64(ls.Size()) != df.EndByte-df.StartByte {
			s.t.Errorf("Split File: %q\n\t  Got Size: %d Expect: %d\n", df.Path, ls.Size, df.EndByte-df.StartByte)
		}
	}
}

// checkSha1Sum will check the sha1 sums of all the destination files
func (s *syncTest) checkSha1Sum(f *File) {
	// Check sha1sum for source file
	eSum, err := sha1sum(f.Path)
	if err != nil {
		s.t.Errorf("Error: No errors from CalcSha1Sum()\n\t Got Sha1Sum: %s", err)
		return
	}
	if eSum != f.Sha1Sum {
		s.t.Errorf("Error: File: %q\n\t  Expect Sha1Sum: %q\n\t  Got Sha1Sum: %q", f.Name, eSum, f.Sha1Sum)
	}
	Log.Debugf("%s sha1sum %q, got f.sha1sum: %q", f.Name, eSum, f.Sha1Sum)

	// Check sha1sum for each dest file
	for _, df := range f.DestFiles {
		sum, err := sha1sum(df.Path)
		if err != nil {
			s.t.Error(err)
			continue
		}
		if df.Sha1Sum != sum {
			s.t.Errorf("Error: DestFile: %q\n\t  Expect Sha1Sum: %q\n\t  Got Sha1Sum: %q",
				df.Path, df.Sha1Sum, sum)
		}
	}

	// Sha1 sum of source file should not be similar to sha1 sum of dest files if the file is split
	if len(f.DestFiles) > 1 {
		for _, df := range f.DestFiles {
			if f.Sha1Sum == df.Sha1Sum {
				s.t.Errorf("Error: Source file %q and dest file have same sha1 sum!", f.Name)
			}
		}
	}
}

// checkSplitFile will check the sha1sum of a file that has been split across devices.
func (s *syncTest) checkMergedSplitFileSha1Sum(f *File) {
	ss := sha1.New()
	for _, df := range f.DestFiles {
		pf, err := os.Open(df.Path)
		if err != nil {
			s.t.Fatal(err)
		}
		if _, err := io.Copy(ss, pf); err != nil {
			s.t.Fatal(err)
		}
	}
	cSum := hex.EncodeToString(ss.Sum(nil))
	eSum, err := sha1sum(f.Path)
	if err != nil {
		s.t.Errorf("Error: No errors from CalcSha1Sum()\n\t Got Sha1Sum: %s", err)
		return
	}
	if eSum != cSum {
		s.t.Errorf("EXPECT: sha1: %q\n\t GOT: %q", eSum, cSum)
	}
	Log.Infof("sha1 %q matched for split file copy %q", cSum, f.Name)

	s.checkSplitSizes(f)
}

// checkSplitSizes will double check the size calculation of split files.
func (s *syncTest) checkSplitSizes(f *File) {
	for _, df := range f.DestFiles {
		if df.EndByte-df.StartByte != df.Size {
			s.t.Errorf("EXPECT: destFile.Size: %d GOT: %d", df.EndByte-df.StartByte, df.Size)
		}
	}
}

// checkMountPointSizes calculates the sizes of the mountpoints for each device on disk and checks against expected values.
func (s *syncTest) checkMountPointSizes() {
	check := func(path string) uint64 {
		var byts uint64
		walkFunc := func(p string, i os.FileInfo, err error) error {
			if p == path {
				return nil
			}
			Log.Debugf("checkMountPointSizes: Got size bytes %d for %q", i.Size(), p)
			byts += uint64(i.Size())
			return nil
		}
		err := filepath.Walk(path, walkFunc)
		if err != nil {
			s.t.Fatal(err)
		}
		return byts
	}
	for _, dev := range s.ctx.Devices {
		ms := check(dev.MountPoint)
		if uint64(ms) > dev.SizeTotal {
			s.t.Errorf("Mountpoint %q usage (%d bytes) is greater than device size (%d bytes)",
				dev.MountPoint, ms, dev.SizeTotal)
		}
		if uint64(ms) != dev.SizeWritn {
			var sCalc uint64
			if s.ctx.SyncContextSize != 0 && dev.Name == s.ctx.Devices[len(s.ctx.Devices)-1].Name {
				if uint64(ms) != (dev.SizeWritn + s.ctx.SyncContextSize) {
					sCalc = dev.SizeWritn + s.ctx.SyncContextSize
				} else {
					continue
				}
			} else {
				sCalc = dev.SizeWritn
			}
			s.t.Errorf("MountPoint: %q\n\t  Got Size: %d dev.SizeWritn: %d\n", dev.MountPoint, ms, sCalc)
		}
	}
}

// progressDump receives from the various sync progress and device mount channels and dumps the value.
func (s *syncTest) progressDump() {
	for x := 0; x < len(s.ctx.Devices); x++ {
		s.ctx.SyncDeviceMount[x] = make(chan bool)
	}
	for x := 0; x < len(s.ctx.Devices); x++ {
		go func(index int) {
			for {
				select {
				case <-s.ctx.SyncDeviceMount[index]:
					s.ctx.SyncDeviceMount[index] <- true
				case <-s.ctx.SyncProgress.Device[index].Report:
				case <-s.ctx.SyncProgress.Report:
				}
			}
		}(x)
	}
}

func (s *syncTest) errorCollector() {
	s.errChan = &s.ctx.Errors
	go func() {
		for {
			e, ok := <-*s.errChan
			if !ok {
				return
			}
			Log.Errorln(e)
			s.errors = append(s.errors, e)
		}
	}()
}

func (s *syncTest) printMountPoints() {
	for _, d := range s.ctx.Devices {
		Log.WithFields(logrus.Fields{"deviceName": d.Name, "mountPoint": d.MountPoint}).Print("Test mountpoint")
	}
}

func (s *syncTest) prepareFileIndex() (FileIndex, DeviceList) {
	if s.fileIndex == nil {
		s.fileIndex = func() FileIndex {
			return FileIndex{}
		}
	}
	files := s.fileIndex()
	devs := s.deviceList()
	return files, devs
}

func (s *syncTest) calcSha1Sum(files FileIndex) {
	for _, f := range files {
		if strings.Contains(f.Path, fakeTestPath) {
			continue
		}
		if f.FileType != FILE {
			continue
		}
		eSum, err := sha1sum(f.Path)
		if err != nil {
			*s.errChan <- err
		}
		f.Sha1Sum = eSum
	}
}

// run intiates the test sync
func (s *syncTest) run() {
	fi, dl := s.prepareFileIndex()

	c, err := NewContext(s.backupPath, s.outputStreams, fi, dl, s.paddingPercentage)
	if err != nil {
		s.errors = append(s.errors, err)
		return
	}
	s.ctx = c

	s.errorCollector()

	s.calcSha1Sum(c.FileIndex)

	if s.dumpFileIndex {
		spd.Dump(c.FileIndex)
		os.Exit(1)
	}

	s.progressDump()

	// DO IT NOW!!
	if s.saveSyncContext {
		Sync(c, false)
	} else {
		Sync(c, true)
	}
}

// Run initiates the test
func (s *syncTest) Run() {
	s.run()
	// Slowdown, give the errorCollector a chance to process any errors
	time.Sleep(time.Millisecond)
	s.checkErrors()
	if s.expectErrors != nil {
		// If checkErrors() did not fail with expectErrors() set, then the expected errors have been found and the
		// test is successful.
		return
	}
	// Check the work for each file
	for _, file := range s.ctx.FileIndex {
		if file.FileType == DIRECTORY || file.FileType == SYMLINK || file.Owner != os.Getuid() {
			continue
		}
		s.checkPerms(file)
		if s.t.Failed() {
			continue
		}
		// Check sha1 of dest files
		s.checkSha1Sum(file)
		if len(file.DestFiles) > 1 {
			s.checkMergedSplitFileSha1Sum(file)
		}
		s.checkDestSize(file)
	}
	s.checkMountPointSizes()
	s.printMountPoints()
}

func TestSyncSimpleCopy(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_freebooks/",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  28173338480,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
			}
		},
	}
	f.Run()
}

// TestSyncSimpleCopySourceFileError attempts to backup a file a regular user can't read. This should generate errors.
func TestSyncSimpleCopySourceFileError(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "/root/",
		fileIndex: func() FileIndex {
			return FileIndex{
				&File{
					Name:     "file",
					FileType: FILE,
					Size:     1024,
					Path:     "/root/file",
					Mode:     0644,
					ModTime:  time.Now(),
					Owner:    os.Getuid(),
					Group:    os.Getgid(),
				},
			}
		},
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  42971520,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
			}
		},
		expectErrors: func() []error {
			return []error{FileSourceNotReadable{}, errors.New("Permission denied")}
		},
	}
	f.Run()
}

// TestSyncSimpleCopyDestPathError should generate on error when attempting to write to un-writable mount point.
func TestSyncSimpleCopyDestPathError(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: fakeTestPath,
		fileIndex: func() FileIndex {
			return FileIndex{
				&File{
					Name:     "testfile",
					FileType: FILE,
					Path:     path.Join(fakeTestPath, "testfile"),
					Mode:     0444,
					ModTime:  time.Now(),
					Owner:    os.Getuid(),
					Group:    os.Getgid(),
					DestFiles: []*DestFile{
						&DestFile{
							Size: 41971520,
							Path: path.Join("/root/mount", "testfile"),
						},
					},
				},
			}
		},
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  42971520,
					MountPoint: "/root/mount",
				},
			}
		},
		expectErrors: func() []error {
			return []error{SyncDestinatonFileOpenError{}}
		},
	}
	f.Run()
}

// TestSyncPerms expects errors when attempting to backup a file to the mock device.
func TestSyncPerms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test")
	}
	testOutputDir := NewMountPoint(t, testTempDir, "mountpoint-0-")
	f := &syncTest{t: t,
		backupPath: fakeTestPath,
		fileIndex: func() FileIndex {
			return FileIndex{
				&File{
					Name:     "diff_user",
					FileType: FILE,
					Path:     path.Join(fakeTestPath, "diff_user"),
					Mode:     0640,
					ModTime:  time.Now(),
					Owner:    55000,
					Group:    55000,
					DestFiles: []*DestFile{
						&DestFile{
							Size: 1024,
							Path: path.Join(testOutputDir, "diff_user"),
						},
					},
				},
				&File{
					Name:     "script.sh",
					FileType: FILE,
					Path:     path.Join(fakeTestPath, "script.sh"),
					Mode:     0777,
					ModTime:  time.Now(),
					Owner:    os.Getuid(),
					Group:    os.Getgid(),
					DestFiles: []*DestFile{
						&DestFile{
							Size: 1024,
							Path: path.Join(testOutputDir, "script.sh"),
						},
					},
				},
				&File{
					Name:     "some_dir",
					Path:     path.Join(fakeTestPath, "some_dir"),
					FileType: DIRECTORY,
					Mode:     0755,
					ModTime:  time.Now(),
					Owner:    os.Getuid(),
					Group:    55000,
					DestFiles: []*DestFile{
						&DestFile{
							Size: 4096,
							Path: path.Join(testOutputDir, "some_dir"),
						},
					},
				},
			}
		},
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  28173338480,
					MountPoint: testOutputDir,
				},
			}
		},
		expectErrors: func() []error {
			return []error{SyncIncorrectOwnershipError{}, errors.New("Some metadata error")}
		},
	}
	f.Run()
}

func TestSyncSubDirs(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_directories/subdirs",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  28173338480,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
			}
		},
	}
	f.Run()
	numDestFiles := 0
	for _, y := range f.ctx.FileIndex {
		if len(y.DestFiles) > 0 {
			numDestFiles += len(y.DestFiles)
		}
	}
	if numDestFiles > 1 {
		t.Errorf("EXPECT: %d destination files GOT: %d", 1, numDestFiles)
	}
}

func TestSyncSymlinks(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_symlinks/",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  28173338480,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
			}
		},
	}
	f.Run()
	if f.ctx.FileIndex[1].SymlinkTarget != "../../testdata/filesync_symlinks/test.txt" {
		t.Errorf("EXPECT: %s GOT: %s", "../../testdata/filesync_symlinks/test.txt",
			f.ctx.FileIndex[1].SymlinkTarget)
	}
	numDestFiles := 0
	for _, y := range f.ctx.FileIndex {
		if len(y.DestFiles) >= 1 {
			numDestFiles += len(y.DestFiles)
		}
	}
	if numDestFiles > 1 {
		t.Errorf("EXPECT: %d destination files GOT: %d", 1, numDestFiles)
	}
}

func TestSyncBackupathIncluded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test")
	}
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_freebooks",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  28173338480,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
			}
		},
	}
	f.Run()
}

// TestSyncFileSplitAcrossDevices is the most important test. It checks syncing a large file across multiple devices.
func TestSyncFileSplitAcrossDevices(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_freebooks",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  1493583,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  1100000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
			}
		},
	}
	// f.dumpFileIndex = true
	f.Run()
}

// TestSyncFileSplitAcrossDevices2 uses of the first device without splitting files. The next two devices contain split
// files.
func TestSyncFileSplitAcrossDevices2(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_freebooks",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  675467,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  990000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
				&Device{
					Name:       "Test Device 2",
					SizeTotal:  990000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-2-"),
				},
			}
		},
	}
	f.Run()
}

// TestSyncFileSplitAcrossDevices3 tests splitting a file starting from the end of the first device into the second device.
func TestSyncFileSplitAcrossDevices3(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_freebooks",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  1575006 + 15751, // Size needed plus 1% for padding
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  4575006,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
			}
		},
	}
	// f.dumpFileIndex = true
	f.Run()
}

// TestSyncFileSplitAcrossDevices4 tests sha1 calculation for a file that is split across devices with first split being
// larger than second.
func TestSyncFileSplitAcrossDevices4(t *testing.T) {
	f := &syncTest{t: t,
		backupPath:    "../../testdata/filesync_large_binary_file",
		outputStreams: 2,
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  8991689 + 89905, // Size needed plus 1% for padding
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  1495254 + 14952,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
			}
		},
	}
	// f.dumpFileIndex = true
	f.Run()
}

// TestSyncAcrossDevicesNoSplit uses two devices with no file splitting.
func TestSyncAcrossDevicesNoSplit(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_freebooks",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  675467,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  1831720,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
			}
		},
	}
	f.Run()
}

// TestSyncFileSplitAcrossDevicesWithProgress copies 41MB from /dev/urandom to a backup file. This test should use the
// progress reporting code without any errors.
func TestSyncFileSplitAcrossDevicesWithProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test")
	}
	f := &syncTest{t: t,
		backupPath: fakeTestPath,
		fileIndex: func() FileIndex {
			return FileIndex{
				&File{
					Name:     "a_large_file",
					FileType: FILE,
					Size:     10485760,
					Path:     "../../testdata/filesync_large_binary_file/a_large_file",
					Mode:     0644,
					ModTime:  time.Now(),
					Owner:    os.Getuid(),
					Group:    os.Getgid(),
				},
			}
		},
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  3495253 + 35953, // Size needed plus 1% for padding.
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  3495253 + 35953, // Size needed plus 1% for padding.
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
				&Device{
					Name:       "Test Device 2",
					SizeTotal:  3495253 + 35953, // Size needed plus 1% for padding.
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-2-"),
				},
			}
		},
	}
	f.Run()
}

func TestSyncLargeFileAcrossOneWholeDeviceAndHalfAnother(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_large_binary_file/",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  9999999,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  850000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
			}
		},
		expectDeviceUsage: func() []expectDevice {
			return []expectDevice{
				expectDevice{
					name:      "Test Device 0",
					usedBytes: 9900000,
				},
				expectDevice{
					name:      "Test Device 1",
					usedBytes: 585760,
				},
			}
		},
	}
	f.Run()
}

func TestSyncLargeFileAcrossThreeDevices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test")
	}
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_large_binary_file",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  3600000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  3600000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
				&Device{
					Name:       "Test Device 2",
					SizeTotal:  3600000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-2-"),
				},
			}
		},
		expectDeviceUsage: func() []expectDevice {
			return []expectDevice{
				expectDevice{
					name:      "Test Device 0",
					usedBytes: 3564000,
				},
				expectDevice{
					name:      "Test Device 1",
					usedBytes: 3564000,
				},
				expectDevice{
					name:      "Test Device 2",
					usedBytes: 3357760,
				},
			}
		},
	}
	f.Run()
}

// TestSyncDirsWithLotsOfFiles checks syncing directories with thousands of files and directories that _had_ thousands of
// files. Directories that had thousands of files are still allocated in the filesystem as containing thousands of file
// pointers, but since filesystems don't reclaim space for deleted directories, recreating these directories on the
// destination drive will allocate the blocksize of the device (4096 bytes for EXT4).
func TestSyncDirsWithLotsOfFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test")
	}
	testTempDir, err := ioutil.TempDir(testTempDir, "gds-filetests-")
	if err != nil {
		t.Error(err)
	}
	// Copy filesync_test09_directories to the temp dir and delete all of the files in the dir
	cmd := exec.Command("/usr/bin/cp", "-R", "../../testdata/filesync_directories", testTempDir)
	err = cmd.Run()
	if err != nil {
		t.Error(err)
	}
	// Duplicate the sub dir
	cmd = exec.Command("/usr/bin/cp", "-R", path.Join(testTempDir, "filesync_directories", "dir_with_thousand_files"),
		path.Join(testTempDir, "filesync_directories", "dir_with_thousand_files_deleted"))
	err = cmd.Run()
	if err != nil {
		t.Error(err)
	}
	// Delete all of the files in the directory
	files, err := filepath.Glob(path.Join(testTempDir, "filesync_directories", "dir_with_thousand_files_deleted", "*"))
	if err != nil {
		t.Error(err)
	}
	for _, y := range files {
		err := os.Remove(y)
		if err != nil {
			t.Errorf("EXPECT: No errors GOT: Error: %s", err)
		}

	}

	f := &syncTest{t: t,
		backupPath: path.Join(testTempDir, "filesync_directories"),
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  4300000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
			}
		},
		expectDeviceUsage: func() []expectDevice {
			return []expectDevice{
				expectDevice{
					name:      "Test Device 0",
					usedBytes: 4096000,
				},
			}
		},
	}
	f.Run()
}

func TestSyncLargeFileNotEnoughDeviceSpace(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_large_binary_file",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  3499350,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  3499350,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
				&Device{
					Name:       "Test Device 2",
					SizeTotal:  300000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-2-"),
				},
			}
		},
		expectErrors: func() []error {
			return []error{DevicePoolSizeExceeded{}}
		},
	}
	f.Run()
}

// TestSyncDestPathMd5Sum does what???
func TestSyncDestPathMd5Sum(t *testing.T) {
	f := &syncTest{t: t,
		backupPath: "../../testdata/filesync_freebooks/alice/",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  769000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
			}
		},
	}
	f.Run()
}

func TestSyncSaveContextLastDevice(t *testing.T) {
	f := &syncTest{t: t,
		backupPath:      "../../testdata/filesync_freebooks",
		saveSyncContext: true,
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  1493583,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  1020000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
			}
		},
	}
	f.Run()
}

func TestSyncSaveContextLastDeviceNotEnoughSpaceError(t *testing.T) {
	f := &syncTest{t: t,
		saveSyncContext: true,
		backupPath:      "../../testdata/filesync_freebooks",
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  1493370,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  1013000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
			}
		},
		expectErrors: func() []error {
			return []error{
				SyncNotEnoughDeviceSpaceForSyncContextError{},
			}
		},
	}
	f.Run()
}

// TestSyncFromTempDirectory copies the testdata to the temp directory. This has the effect of reducing file sizes to their
// actual size. Once this is done, a sync is made to a real file system which creates small files using 1 block size.
func TestSyncFromTempDirectory(t *testing.T) {
	p, err := ioutil.TempDir("", "gds-freebooks-")
	if err != nil {
		t.Fatalf("EXPECT: path to temp mount GOT: %s", err)
	}
	Log.WithFields(logrus.Fields{"path": p}).Print("filesync_freebooks temporary path")
	cmd := exec.Command("/usr/bin/cp", "-R", "../../testdata/filesync_freebooks", p)
	err = cmd.Run()
	if err != nil {
		t.Fatalf("EXPECT: No copy errors GOT: %s", err)
	}
	f := &syncTest{t: t,
		backupPath: p,
		deviceList: func() DeviceList {
			return DeviceList{
				&Device{
					Name:       "Test Device 0",
					SizeTotal:  1000000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-0-"),
				},
				&Device{
					Name:       "Test Device 1",
					SizeTotal:  1000000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-1-"),
				},
				&Device{
					Name:       "Test Device 2",
					SizeTotal:  1000000,
					MountPoint: NewMountPoint(t, testTempDir, "mountpoint-2-"),
				},
			}
		},
	}
	f.Run()
}
