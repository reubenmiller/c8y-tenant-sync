package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newBundleCmd() *cobra.Command {
	var manifestFlag, output string
	var extra []string
	cmd := &cobra.Command{
		Use:   "bundle [manifest.yaml] -o bundle.tar.gz",
		Short: "Package a manifest with its files into a bundle",
		Long: `Package a manifest and the local files it uses into a bundle archive (.tar.gz,
.tgz or .zip), which other manifests include from a path, a URL or a GitHub release
asset, or which is applied directly ("run bundle.tar.gz").

The bundle contains the manifest, the manifests it includes, and the files of its
local sources (source.path). Includes from URLs and GitHub releases stay remote.
Files used by hooks and commands (e.g. scripts) are added with --add. All files
must be in the manifest directory or below it.`,
		Example: formatExamples(
			example{"bundle tenant.yaml -o blueprint.tar.gz", "Package tenant.yaml with the files it uses"},
			example{"bundle prod.yaml -o blueprint.zip --add scripts", "Also add the scripts directory used by hooks"},
		),
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeManifestFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			if manifestFlag != "" && len(args) > 0 {
				return fmt.Errorf("specify the manifest either with -f or as an argument, not both")
			}
			manifestPath := manifestFlag
			if len(args) > 0 {
				manifestPath = args[0]
			}
			if manifestPath == "" {
				manifestPath = "tenant.yaml"
			}
			return runBundle(manifestPath, output, extra)
		},
	}
	cmd.Flags().StringVarP(&manifestFlag, "manifest", "f", "", "Path to the tenant manifest file (default: tenant.yaml)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Bundle file to write (.tar.gz, .tgz or .zip)")
	cmd.Flags().StringArrayVar(&extra, "add", nil, "Also add a file or directory (e.g. scripts used by hooks); repeatable")
	_ = cmd.MarkFlagRequired("output")
	_ = cmd.RegisterFlagCompletionFunc("manifest", completeManifestFiles)
	return cmd
}

// runBundle packages a manifest with its local files into a bundle archive
func runBundle(manifestPath, output string, extra []string) error {
	if isRemoteManifest(manifestPath) || isBundleArchive(manifestPath) || isStdinManifest(manifestPath) {
		return fmt.Errorf("%s: bundle a local manifest file", manifestPath)
	}
	if !isBundleArchive(output) {
		return fmt.Errorf("output %s must end with .tar.gz, .tgz or .zip", output)
	}
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(abs)
	outputAbs, err := filepath.Abs(output)
	if err != nil {
		return err
	}

	manifest, loader, err := loadManifests([]string{manifestPath}, LoadOptions{AllowRemoteCommands: true})
	if err != nil {
		return err
	}
	defer manifest.Cleanup()

	files := map[string]bool{}
	for _, file := range loader.localFiles {
		files[file] = true
	}

	// The files of the local sources (paths are relative to the manifest
	// directory after loading). Sources of bundles included from a local
	// archive are in that archive.
	resolver := NewSourceResolver(root, "", true)
	var errs []string
	manifest.forEachSource(func(label string, source *Source) {
		if source.Path == "" {
			return
		}
		target := source.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		if loader.inBundle(target) {
			return
		}
		resolved, err := resolver.resolveLocal(*source)
		if err != nil {
			if source.Optional && errors.Is(err, ErrNothingToDo) {
				return
			}
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			return
		}
		for _, file := range resolved {
			files[file.Path] = true
		}
	})
	if len(errs) > 0 {
		return fmt.Errorf("failed to collect the source files:\n  - %s", strings.Join(errs, "\n  - "))
	}

	for _, item := range extra {
		itemAbs, err := filepath.Abs(item)
		if err != nil {
			return err
		}
		err = filepath.WalkDir(itemAbs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				files[path] = true
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("--add %s: %w", item, err)
		}
	}

	// Secrets stay out of the bundle, and so does the bundle itself
	delete(files, DefaultSecretsPath(abs))
	delete(files, outputAbs)

	names := make([]string, 0, len(files))
	sources := map[string]string{}
	var outside []string
	for file := range files {
		if !isWithin(root, file) {
			outside = append(outside, file)
			continue
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		names = append(names, name)
		sources[name] = file
	}
	if len(outside) > 0 {
		sort.Strings(outside)
		return fmt.Errorf("a bundle only contains files in %s or below it; these files are outside:\n  - %s", root, strings.Join(outside, "\n  - "))
	}
	sort.Strings(names)

	entry := filepath.ToSlash(filepath.Base(abs))
	marker := []byte(fmt.Sprintf("# Entry manifest of this tenant-sync bundle\nmanifest: %s\n", entry))
	if err := writeBundle(outputAbs, marker, names, sources); err != nil {
		return fmt.Errorf("failed to write %s: %w", output, err)
	}

	checksum, err := fileSHA256(outputAbs)
	if err != nil {
		return err
	}
	fmt.Printf("✓ Wrote %s (%d files, entry manifest %s)\n", output, len(names), entry)
	for _, name := range names {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("  sha256: %s\n", checksum)
	return nil
}

// bundleModTime is the modification time of all bundle entries, so the same
// files always give the same archive
var bundleModTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// writeBundle writes the marker file and the files (archive name -> local
// path) in the given order
func writeBundle(output string, marker []byte, names []string, sources map[string]string) error {
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	var writeErr error
	if strings.HasSuffix(strings.ToLower(output), ".zip") {
		writeErr = writeZipBundle(out, marker, names, sources)
	} else {
		writeErr = writeTarGzBundle(out, marker, names, sources)
	}
	if err := out.Close(); writeErr == nil {
		writeErr = err
	}
	if writeErr != nil {
		_ = os.Remove(output)
	}
	return writeErr
}

// bundleFileMode returns the archive mode of a file: executable files keep
// their executable bit
func bundleFileMode(info os.FileInfo) int64 {
	if info.Mode()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func writeTarGzBundle(out io.Writer, marker []byte, names []string, sources map[string]string) error {
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	header := func(name string, mode, size int64) *tar.Header {
		return &tar.Header{Name: name, Mode: mode, Size: size, ModTime: bundleModTime, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	}
	if err := tw.WriteHeader(header(BundleMarkerFile, 0o644, int64(len(marker)))); err != nil {
		return err
	}
	if _, err := tw.Write(marker); err != nil {
		return err
	}
	for _, name := range names {
		err := func() error {
			file, err := os.Open(sources[name])
			if err != nil {
				return err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(header(name, bundleFileMode(info), info.Size())); err != nil {
				return err
			}
			_, err = io.Copy(tw, file)
			return err
		}()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZipBundle(out io.Writer, marker []byte, names []string, sources map[string]string) error {
	zw := zip.NewWriter(out)
	create := func(name string, mode int64) (io.Writer, error) {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: bundleModTime}
		header.SetMode(os.FileMode(mode))
		return zw.CreateHeader(header)
	}
	w, err := create(BundleMarkerFile, 0o644)
	if err != nil {
		return err
	}
	if _, err := w.Write(marker); err != nil {
		return err
	}
	for _, name := range names {
		err := func() error {
			file, err := os.Open(sources[name])
			if err != nil {
				return err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return err
			}
			w, err := create(name, bundleFileMode(info))
			if err != nil {
				return err
			}
			_, err = io.Copy(w, file)
			return err
		}()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}
