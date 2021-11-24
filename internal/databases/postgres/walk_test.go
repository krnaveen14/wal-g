package postgres_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/internal/databases/postgres"
	"github.com/wal-g/wal-g/testtools"
	"github.com/wal-g/wal-g/utility"
)

const BUFSIZE = 4 * 1024

// Generates fake `backup_label` and `tablespace_map` files
// that are usually generated by postgres.
func createLabelFiles(t *testing.T, dir string) {
	l, err := os.Create(filepath.Join(dir, "backup_label"))
	if err != nil {
		t.Log(err)
	}
	err = l.Chmod(0600)
	if err != nil {
		t.Log(err)
	}

	_, err = l.WriteString("backup")
	if err != nil {
		t.Log(err)
	}

	s, err := os.Create(filepath.Join(dir, "tablespace_map"))
	if err != nil {
		t.Log(err)
	}
	err = s.Chmod(0600)
	if err != nil {
		t.Log(err)
	}

	_, err = s.WriteString("table")
	if err != nil {
		t.Log(err)
	}

	defer utility.LoggedClose(l, "")
	defer utility.LoggedClose(s, "")
}

// Generate 5 1MB of random data and write to temp
// directory 'data...'. Also creates a fake sentinel file and
// tests that excluded directories are handled correctly.
func generateData(t *testing.T) string {
	cwd, err := filepath.Abs("./")
	if err != nil {
		t.Log(err)
	}

	// Create temp directory.
	dir, err := ioutil.TempDir(cwd, "data")
	if err != nil {
		t.Log(err)
	}
	fmt.Println(dir)

	sb := testtools.NewStrideByteReader(10)

	// Generates 5 1MB files
	for i := 1; i < 6; i++ {
		lr := &io.LimitedReader{
			R: sb,
			N: int64(100),
		}
		f, err := os.Create(filepath.Join(dir, strconv.Itoa(i)))
		if err != nil {
			t.Log(err)
		}
		io.Copy(f, lr)
		defer utility.LoggedClose(f, "")
	}

	// Make sentinel
	err = os.MkdirAll(filepath.Join(dir, "global"), 0700)
	if err != nil {
		t.Log(err)
	}

	f, err := os.Create(filepath.Join(dir, "global", postgres.PgControl))
	if err != nil {
		t.Log(err)
	}
	err = f.Chmod(0600)
	if err != nil {
		t.Log(err)
	}

	// Test that concurrency doesn't break extract.
	s, err := os.Create(filepath.Join(dir, "global", "bytes"))
	if err != nil {
		t.Log(err)
	}
	err = s.Chmod(0600)
	if err != nil {
		t.Log(err)
	}

	// Generate large enough file (500MB) so that goroutine doesn't finish before extracting pg_control
	lr := &io.LimitedReader{
		R: sb,
		N: int64(500 * 1024 * 1024),
	}
	_, err = io.Copy(s, lr)
	if err != nil {
		t.Log(err)
	}

	defer utility.LoggedClose(f, "")
	defer utility.LoggedClose(s, "")

	// Create excluded directory with one file in it.
	err = os.MkdirAll(filepath.Join(dir, "pg_notify"), 0700)
	if err != nil {
		t.Log(err)
	}
	n, err := os.Create(filepath.Join(dir, "pg_notify", "0000"))
	if err != nil {
		t.Log(err)
	}
	err = n.Chmod(0600)
	if err != nil {
		t.Log(err)
	}
	defer utility.LoggedClose(n, "")

	// Create `backup_label` and `tablespace_map` files.
	createLabelFiles(t, dir)

	return dir
}

// Extract files to temp directory 'extracted'.
func extract(t *testing.T, dir string) string {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		t.Log(err)
	}

	out := make([]internal.ReaderMaker, len(files))
	for i, file := range files {
		filePath := filepath.Join(dir, file.Name())
		f := &testtools.FileReaderMaker{
			Key: filePath,
		}
		out[i] = f
	}

	outDir := filepath.Join(filepath.Dir(dir), "extracted")

	ft := postgres.NewFileTarInterpreter(outDir, postgres.BackupSentinelDto{}, postgres.FilesMetadataDto{}, map[string]bool{
		"/1":                 true,
		"/2":                 true,
		"/3":                 true,
		"/4":                 true,
		"/5":                 true,
		"/backup_label":      true,
		"/global/bytes":      true,
		"/global/pg_control": true,
		"/pg_notify/0000":    true,
		"/tablespace_map":    true,
	}, false)
	err = os.MkdirAll(outDir, 0766)
	if err != nil {
		t.Log(err)
	}

	err = internal.ExtractAll(ft, out)
	if err != nil {
		t.Log(err)
	}

	return outDir

}

// First compares two directories and returns true if everything in
// os.FileInfo is the same except for FileInfo.Sys() (syscall.Stat_t)
// and ModTimes. If initial comparison returns true, compares first
// 4KB of file content and will only compute sha256 for both files if the
// initial bytes are the same.
func compare(t *testing.T, dir1, dir2 string) bool {
	// ReadDir returns directory by filename.
	files1, err := ioutil.ReadDir(dir1)
	if err != nil {
		t.Log(err)
	}

	files2, err := ioutil.ReadDir(dir2)
	if err != nil {
		t.Log(err)
	}

	// Compares os.FileInfo without syscall.Stat_t fields or ModTimes.
	var shallowEqual bool
	var deepEqual bool
	for i, f2 := range files2 {
		f1 := files1[i]
		name := f1.Name() == f2.Name()
		size := f1.Size() == f2.Size()
		mode := f1.Mode() == f2.Mode()
		isDir := f1.IsDir() == f2.IsDir()

		// If directory is in ExcludedFilenames list, make sure it exists but is empty.
		if f2.IsDir() {
			_, ok := postgres.ExcludedFilenames[f2.Name()]
			if ok {
				size = isEmpty(t, filepath.Join(dir2, f2.Name()))
			}
		}

		shallowEqual = name && size && mode && isDir

		// If directories are the same, compares contents of the files.
		if shallowEqual {
			if !(f1.IsDir() && f2.IsDir()) {
				f1Path := filepath.Join(dir1, f1.Name())
				f2Path := filepath.Join(dir2, f2.Name())
				f1Dir := filepath.Base(dir1)
				f2Dir := filepath.Base(dir2)

				deepEqual = computeSha(t, f1Path, f2Path)

				if !deepEqual {
					t.Logf("walk: files %s in %s and %s in %s are different.", f1.Name(), f1Dir, f2.Name(), f2Dir)
				}

			}
		} else {
			t.Logf("walk: Original: \t%s\t %d\t %d\t %v", f1.Name(), f1.Size(), f1.Mode(), f1.IsDir())
			t.Logf("walk: Extracted: \t%s\t %d\t %d\t %v", f2.Name(), f2.Size(), f2.Mode(), f2.IsDir())
		}

	}

	return deepEqual && shallowEqual
}

// Computes the sha256 of FILE1 and FILE2. Will only
// compute the sum if the first 4KB of the two files
// are the same.
func computeSha(t *testing.T, file1, file2 string) bool {
	f1, err := os.Open(file1)
	if err != nil {
		t.Log(err)
	}

	f2, err := os.Open(file2)
	if err != nil {
		t.Log(err)
	}

	defer utility.LoggedClose(f1, "")
	defer utility.LoggedClose(f2, "")

	// Check if first 4KB of files are the same.
	buf1 := make([]byte, BUFSIZE)
	buf2 := make([]byte, BUFSIZE)

	l1 := &io.LimitedReader{
		R: f1,
		N: BUFSIZE,
	}
	l2 := &io.LimitedReader{
		R: f2,
		N: BUFSIZE,
	}

	l1.Read(buf1)
	l2.Read(buf2)

	equal := bytes.Equal(buf1, buf2)

	// If 4KB of files are equal, proceed to sha256 computation, else quit early.
	if equal {
		// Start readers from beginning of files.
		f1.Seek(0, 0)
		f2.Seek(0, 0)

		// Compute sha256 based on entire file.
		h1 := sha256.New()
		_, err = io.Copy(h1, f1)
		if err != nil {
			t.Log(err)
		}

		h2 := sha256.New()
		_, err = io.Copy(h2, f2)
		if err != nil {
			t.Log(err)
		}

		equal = bytes.Equal(h1.Sum(nil), h2.Sum(nil))
	}

	return equal

}

// Check if directory is empty. Used to test behavior
// of excluded directories.
func isEmpty(t *testing.T, path string) bool {
	f, err := os.Open(path)
	if err != nil {
		t.Log(err)
	}
	defer utility.LoggedClose(f, "")
	_, err = f.Readdirnames(1)
	return err == io.EOF
}

func TestWalk_RegularComposer(t *testing.T) {
	testWalk(t, postgres.RegularComposer, false)
}

func TestWalk_RegularComposerWithoutFilesMetadata(t *testing.T) {
	testWalk(t, postgres.RegularComposer, true)
}

func TestWalk_RatingComposer(t *testing.T) {
	testWalk(t, postgres.RatingComposer, false)
}

func TestWalk_CopyComposer(t *testing.T) {
	testWalk(t, postgres.CopyComposer, false)
}

func testWalk(t *testing.T, composer postgres.TarBallComposerType, withoutFilesMetadata bool) {
	// Generate random data and write to tmp dir `data...`.
	data := generateData(t)
	tarSizeThreshold := int64(10)
	// Bundle and compress files to `compressed`.
	bundle := postgres.NewBundle(data, nil, nil, nil, false, tarSizeThreshold)
	compressed := filepath.Join(filepath.Dir(data), "compressed")
	size := int64(0)
	tarBallMaker := &testtools.FileTarBallMaker{
		Out:  compressed,
		Size: &size,
	}
	err := os.MkdirAll(compressed, 0766)
	if err != nil {
		t.Log(err)
	}

	err = bundle.StartQueue(tarBallMaker)
	if err != nil {
		t.Log(err)
	}

	err = bundle.SetupComposer(setupTestTarBallComposerMaker(composer, withoutFilesMetadata))
	if err != nil {
		t.Log(err)
	}

	fmt.Println("Walking ...")
	err = filepath.Walk(data, bundle.HandleWalkedFSObject)
	if err != nil {
		t.Log(err)
	}
	tarFileSets, err := bundle.PackTarballs()
	if err != nil {
		t.Log(err)
	}

	backupFileListEmpty := true
	bundle.GetFiles().Range(func(key, value interface{}) bool {
		backupFileListEmpty = false
		return false
	})

	if withoutFilesMetadata {
		// Test tarFileSets is not tracked
		assert.True(t, len(tarFileSets.Get()) == 0)
		// Test BackupFileList is not tracked
		assert.True(t, backupFileListEmpty)
	} else {
		assert.True(t, len(tarFileSets.Get()) > 0)
		assert.False(t, backupFileListEmpty)
	}

	err = bundle.FinishQueue()
	if err != nil {
		t.Log(err)
	}

	// Test that sentinel exists and is handled correctly.
	sen := bundle.Sentinel.Info.Name()
	assert.Equal(t, postgres.PgControl, sen)

	err = bundle.UploadPgControl("lz4")
	assert.NoError(t, err)

	// err = bundle.UploadLabelFiles("backup", "table")
	// if err != nil {
	// 	t.Errorf("walk: Sentinel expected to succeed but got %+v\n", err)
	// }

	// Extracts compressed directory to `extracted`.
	extracted := extract(t, compressed)
	if compare(t, data, extracted) {
		// Clean up only if the test succeeds.
		defer os.RemoveAll(data)
		defer os.RemoveAll(compressed)
		defer os.RemoveAll(extracted)
	} else {
		t.Errorf("walk: Extracted and original directories are not the same.")
	}

	// Re-use generated data to test uploading WAL.
	uploader := testtools.NewMockUploader(false, false)
	walFileName := filepath.Join(data, "1")
	walFile, err := os.Open(walFileName)
	assert.NoError(t, err)
	err = uploader.UploadFile(walFile)

	if err != nil {
		// t.Errorf("upload: expected no error to occur but got %+v", err)
		t.Logf("%+v\n", err)
	}
}

func setupTestTarBallComposerMaker(composer postgres.TarBallComposerType, withoutFilesMetadata bool) postgres.TarBallComposerMaker {
	filePackOptions := postgres.NewTarBallFilePackerOptions(false, false)
	switch composer {
	case postgres.RegularComposer:
		if withoutFilesMetadata {
			return postgres.NewRegularTarBallComposerMaker(filePackOptions, &postgres.NopBundleFiles{}, postgres.NewNopTarFileSets())
		} else {
			return postgres.NewRegularTarBallComposerMaker(filePackOptions, &postgres.RegularBundleFiles{}, postgres.NewRegularTarFileSets())
		}
	case postgres.RatingComposer:
		relFileStats := make(postgres.RelFileStatistics)
		composerMaker, _ := postgres.NewRatingTarBallComposerMaker(relFileStats, filePackOptions)
		return composerMaker
	case postgres.CopyComposer:
		mockBackup := getMockBackupFromFiles(nil)
		return postgres.NewCopyTarBallComposerMaker(mockBackup, "mockName", filePackOptions)
	default:
		return nil
	}
}
