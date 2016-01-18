package commands_helpers

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/common"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers"
)

type ArchiveCommand struct {
	Paths      []string `long:"path" description:"Add paths to archive"`
	Untracked  bool     `long:"untracked" description:"Add git untracked files"`
	File       string   `long:"file" description:"The path to file"`
	Verbose    bool     `long:"verbose" description:"Detailed information"`
	List       bool     `long:"list" description:"List files to archive"`
	NoProgress bool     `long:"no-progress" description:"Disable progress bar"`

	wd         string
	files      map[string]os.FileInfo
	totalSize  int64
}

func isTarArchive(fileName string) bool {
	if strings.HasSuffix(fileName, ".tgz") || strings.HasSuffix(fileName, ".tar.gz") {
		return true
	}
	return false
}

func isZipArchive(fileName string) bool {
	if strings.HasSuffix(fileName, ".zip") {
		return true
	}
	return false
}

func (c *ArchiveCommand) isChanged(modTime time.Time) bool {
	for _, info := range c.files {
		if modTime.Before(info.ModTime()) {
			return true
		}
	}
	return false
}

func (c *ArchiveCommand) sortedFiles() []string {
	files := make([]string, len(c.files))

	i := 0
	for file := range c.files {
		files[i] = file
		i++
	}

	sort.Strings(files)
	return files
}

func (c *ArchiveCommand) add(path string, info os.FileInfo) (err error) {
	if info == nil {
		info, err = os.Lstat(path)
	}
	if err == nil {
		c.files[path] = info
		if info.Mode().IsRegular() {
			c.totalSize += info.Size()
		}
	} else if os.IsNotExist(err) {
		logrus.Warningln("File", path, "doesn't exist")
		err = nil
	}
	return
}

func (c *ArchiveCommand) process(match string) error {
	absolute, err := filepath.Abs(match)
	if err != nil {
		return err
	}

	relative, err := filepath.Rel(c.wd, absolute)
	if err != nil {
		return err
	}

	// store relative path if points to current working directory
	if strings.HasPrefix(relative, ".." + string(filepath.Separator)) {
		return c.add(absolute, nil)
	} else {
		return c.add(relative, nil)
	}
}

func (c *ArchiveCommand) processPaths() {
	for _, path := range c.Paths {
		matches, err := filepath.Glob(path)
		if err != nil {
			logrus.Warningln(err)
			continue
		}

		for _, match := range matches {
			err := filepath.Walk(match, func(path string, info os.FileInfo, err error) error {
				return c.process(path)
			})
			if err != nil {
				logrus.Warningln("Walking", match, err)
			}
		}
	}
}

func (c *ArchiveCommand) processUntracked() {
	if !c.Untracked {
		return
	}

	var output bytes.Buffer
	cmd := exec.Command("git", "ls-files", "-o")
	cmd.Env = os.Environ()
	cmd.Stdout = &output
	cmd.Stderr = os.Stderr
	logrus.Debugln("Executing command:", strings.Join(cmd.Args, " "))
	err := cmd.Run()
	if err == nil {
		reader := bufio.NewReader(&output)
		for {
			line, _, err := reader.ReadLine()
			if err == io.EOF {
				break
			} else if err != nil {
				logrus.Warningln(err)
				break
			}
			c.process(string(line))
		}
	} else {
		logrus.Warningln(err)
	}
}

func (c *ArchiveCommand) listFiles() {
	if len(c.files) == 0 {
		logrus.Infoln("No files to archive.")
		return
	}

	for _, file := range c.sortedFiles() {
		println(string(file))
	}
}

func (c *ArchiveCommand) createZipArchive(w io.Writer, fileNames []string) error {
	archive := zip.NewWriter(w)
	defer archive.Close()

	bar := helpers.NewPbForBytes(c.totalSize)
	if !c.NoProgress {
		bar.Start()
		defer bar.Finish()
	}

	for _, fileName := range fileNames {
		fi, err := os.Lstat(fileName)
		if err != nil {
			logrus.Warningln("File ignored: %q: %v", fileName, err)
			continue
		}

		fh, err := zip.FileInfoHeader(fi)
		fh.Name = fileName
		fh.Extra = createZipExtra(fi)

		switch fi.Mode() & os.ModeType {
		case os.ModeDir:
			fh.Name += "/"

			_, err := archive.CreateHeader(fh)
			if err != nil {
				return err
			}

		case os.ModeSymlink:
			fw, err := archive.CreateHeader(fh)
			if err != nil {
				return err
			}

			link, err := os.Readlink(fileName)
			if err != nil {
				return err
			}

			io.WriteString(fw, link)

		case os.ModeNamedPipe, os.ModeSocket, os.ModeDevice:
			// Ignore the files that of these types
			logrus.Warningln("File ignored: %q", fileName)

		default:
			fh.Method = zip.Deflate
			fw, err := archive.CreateHeader(fh)
			if err != nil {
				return err
			}

			file, err := os.Open(fileName)
			if err != nil {
				return err
			}

			if !c.NoProgress {
				barReader := bar.NewProxyReader(file)
				_, err = io.Copy(fw, barReader)
			} else {
				_, err = io.Copy(fw, file)
			}
			file.Close()
			if err != nil {
				return err
			}
			break
		}

		if c.Verbose {
			fmt.Printf("%v\t%d\t%s\n", fh.Mode(), fh.UncompressedSize64, fh.Name)
		}
	}

	return nil
}

func (c *ArchiveCommand) createTarArchive(w io.Writer, files []string) error {
	var list bytes.Buffer
	for _, file := range c.sortedFiles() {
		list.WriteString(string(file) + "\n")
	}

	flags := "-zcP"
	if c.Verbose {
		flags += "v"
	}

	cmd := exec.Command("tar", flags, "-T", "-", "--no-recursion")
	cmd.Env = os.Environ()
	cmd.Stdin = &list
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	logrus.Debugln("Executing command:", strings.Join(cmd.Args, " "))
	return cmd.Run()
}

func (c *ArchiveCommand) createArchive(w io.Writer, files []string) error {
	if isTarArchive(c.File) {
		return c.createTarArchive(w, files)
	} else if isZipArchive(c.File) {
		return c.createZipArchive(w, files)
	} else {
		return fmt.Errorf("Unsupported archive format: %q", c.File)
	}
}

func (c *ArchiveCommand) archive() {
	if len(c.files) == 0 {
		logrus.Infoln("No files to archive.")
		return
	}

	if c.NoProgress {
		logrus.Infoln("Creating archive", filepath.Base(c.File), "...")
	}

	// create directories to store archive
	os.MkdirAll(filepath.Dir(c.File), 0700)

	tempFile, err := ioutil.TempFile(filepath.Dir(c.File), "archive_")
	if err != nil {
		logrus.Fatalln("Failed to create temporary archive", err)
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())

	logrus.Debugln("Temporary file:", tempFile.Name())
	err = c.createArchive(tempFile, c.sortedFiles())
	if err != nil {
		logrus.Fatalln("Failed to create archive:", err)
	}
	tempFile.Close()

	err = os.Rename(tempFile.Name(), c.File)
	if err != nil {
		logrus.Warningln("Failed to rename archive:", err)
	}

	if c.NoProgress {
		logrus.Infoln("Done!")
	}
}

func (c *ArchiveCommand) Execute(context *cli.Context) {
	logrus.SetFormatter(
		&logrus.TextFormatter{
			ForceColors:      true,
			DisableTimestamp: false,
		},
	)

	if c.Verbose {
		c.List = false
		c.NoProgress = true
	}
	if c.List {
		c.NoProgress = true
	}

	wd, err := os.Getwd()
	if err != nil {
		logrus.Fatalln("Failed to get current working directory:", err)
	}
	if c.File == "" && !c.List {
		logrus.Fatalln("Missing archive file name!")
	}

	c.wd = wd
	c.files = make(map[string]os.FileInfo)

	c.processPaths()
	c.processUntracked()

	ai, err := os.Stat(c.File)
	if err != nil && !os.IsNotExist(err) {
		logrus.Fatalln("Failed to verify archive:", c.File, err)
	}
	if ai != nil {
		if !c.isChanged(ai.ModTime()) {
			logrus.Infoln("Archive is up to date!")
			return
		}
	}

	if c.List {
		c.listFiles()
	} else {
		c.archive()
	}
}

func init() {
	common.RegisterCommand2("archive", "find and archive files (internal)", &ArchiveCommand{})
}
