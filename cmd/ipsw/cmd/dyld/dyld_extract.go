/*
Copyright © 2018-2023 blacktop

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package dyld

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/apex/log"
	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/pkg/fixupchains"
	"github.com/blacktop/ipsw/pkg/dyld"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

func rebaseMachO(dsc *dyld.File, machoPath string) error {
	f, err := os.OpenFile(machoPath, os.O_RDWR, 0755)
	if err != nil {
		return fmt.Errorf("failed to open exported MachO %s: %v", machoPath, err)
	}
	defer f.Close()

	mm, err := macho.NewFile(f)
	if err != nil {
		return err
	}

	for _, seg := range mm.Segments() {
		uuid, mapping, err := dsc.GetMappingForVMAddress(seg.Addr)
		if err != nil {
			return err
		}

		if mapping.SlideInfoOffset == 0 {
			continue
		}

		startAddr := seg.Addr - mapping.Address
		endAddr := ((seg.Addr + seg.Memsz) - mapping.Address) + uint64(dsc.SlideInfo.GetPageSize())

		start := startAddr / uint64(dsc.SlideInfo.GetPageSize())
		end := endAddr / uint64(dsc.SlideInfo.GetPageSize())

		rebases, err := dsc.GetRebaseInfoForPages(uuid, mapping, start, end)
		if err != nil {
			return err
		}

		for _, rebase := range rebases {
			off, err := mm.GetOffset(rebase.CacheVMAddress)
			if err != nil {
				continue
			}
			if _, err := f.Seek(int64(off), io.SeekStart); err != nil {
				return fmt.Errorf("failed to seek in exported file to offset %#x from the start: %v", off, err)
			}
			if err := binary.Write(f, dsc.ByteOrder, rebase.Target); err != nil {
				return fmt.Errorf("failed to write rebase address %#x: %v", rebase.Target, err)
			}
		}
	}

	return nil
}

func init() {
	DyldCmd.AddCommand(dyldExtractCmd)
	dyldExtractCmd.Flags().BoolP("all", "a", false, "Split ALL dylibs")
	dyldExtractCmd.Flags().Bool("force", false, "Overwrite existing extracted dylib(s)")
	dyldExtractCmd.Flags().Bool("slide", false, "Apply slide info to extracted dylib(s)")
	dyldExtractCmd.Flags().StringP("output", "o", "", "Directory to extract the dylib(s)")
	viper.BindPFlag("dyld.extract.all", dyldExtractCmd.Flags().Lookup("all"))
	viper.BindPFlag("dyld.extract.force", dyldExtractCmd.Flags().Lookup("force"))
	viper.BindPFlag("dyld.extract.slide", dyldExtractCmd.Flags().Lookup("slide"))
	viper.BindPFlag("dyld.extract.output", dyldExtractCmd.Flags().Lookup("output"))
}

// dyldExtractCmd represents the extractDyld command
var dyldExtractCmd = &cobra.Command{
	Use:           "extract <DSC> <DYLIB>",
	Short:         "Extract dylib from dyld_shared_cache",
	Args:          cobra.MinimumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {

		var bar *mpb.Bar
		var p *mpb.Progress
		var images []*dyld.CacheImage

		if viper.GetBool("verbose") {
			log.SetLevel(log.DebugLevel)
		}

		// flags
		dumpALL := viper.GetBool("dyld.extract.all")
		forceExtract := viper.GetBool("dyld.extract.force")
		slide := viper.GetBool("dyld.extract.slide")
		output := viper.GetString("dyld.extract.output")
		// validate flags
		if dumpALL && len(args) > 1 {
			return fmt.Errorf("cannot specify DYLIB(s) when using --all")
		} else if !dumpALL && len(args) < 2 {
			return fmt.Errorf("must specify at least one DYLIB to extract")
		}

		dscPath := filepath.Clean(args[0])

		folder := filepath.Dir(dscPath) // default to folder of shared cache
		if len(output) > 0 {
			folder = output
		}

		fileInfo, err := os.Lstat(dscPath)
		if err != nil {
			return fmt.Errorf("file %s does not exist", dscPath)
		}

		// Check if file is a symlink
		if fileInfo.Mode()&os.ModeSymlink != 0 {
			symlinkPath, err := os.Readlink(dscPath)
			if err != nil {
				return errors.Wrapf(err, "failed to read symlink %s", dscPath)
			}
			// TODO: this seems like it would break
			linkParent := filepath.Dir(dscPath)
			linkRoot := filepath.Dir(linkParent)

			dscPath = filepath.Join(linkRoot, symlinkPath)
		}

		f, err := dyld.Open(dscPath)
		if err != nil {
			return err
		}
		defer f.Close()

		if dumpALL {
			// set images to all images in shared cache
			images = f.Images
			// initialize progress bar
			p = mpb.New(mpb.WithWidth(80))
			// adding a single bar, which will inherit container's width
			name := "      "
			bar = p.New(int64(len(images)),
				// progress bar filler with customized style
				mpb.BarStyle().Lbound("[").Filler("=").Tip(">").Padding("-").Rbound("|"),
				mpb.PrependDecorators(
					decor.Name(name, decor.WC{W: len(name) + 1, C: decor.DidentRight}),
					// replace ETA decorator with "done" message, OnComplete event
					decor.OnComplete(
						decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 4}), "✅ ",
					),
				),
				mpb.AppendDecorators(
					decor.CountersNoUnit("%d/%d"),
					decor.Name(" ] "),
				),
			)
			log.Infof("Extracting all dylibs from %s", dscPath)
		} else {
			// get images from args
			images = make([]*dyld.CacheImage, 0, len(args)-1)
			for _, arg := range args[1:] {
				image, err := f.Image(arg)
				if err != nil {
					return err
				}
				images = append(images, image)
			}
		}

		for _, image := range images {
			m, err := image.GetMacho()
			if err != nil {
				return err
			}

			fname := filepath.Join(folder, filepath.Base(image.Name)) // default to NOT full dylib path
			if dumpALL {
				fname = filepath.Join(folder, image.Name)
			}

			if _, err := os.Stat(fname); os.IsNotExist(err) || forceExtract {
				var dcf *fixupchains.DyldChainedFixups
				if m.HasFixups() {
					dcf, err = m.DyldChainedFixups()
					if err != nil {
						return fmt.Errorf("failed to parse fixups from in memory MachO: %v", err)
					}
				}

				image.ParseLocalSymbols(false)

				if err := m.Export(fname, dcf, m.GetBaseAddress(), image.GetLocalSymbolsAsMachoSymbols()); err != nil {
					return fmt.Errorf("failed to extract dylib %s: %v", image.Name, err)
				}
				if slide {
					if err := rebaseMachO(f, fname); err != nil {
						return fmt.Errorf("failed to rebase dylib via cache slide info: %v", err)
					}
				}

				if dumpALL {
					bar.Increment()
				} else {
					log.Infof("Created %s", fname)
				}
			} else {
				if dumpALL {
					bar.Increment()
				} else {
					log.Warnf("Dylib already exists: %s", fname)
				}
			}

			m.Close()
		}

		if dumpALL {
			p.Wait()
		}

		return nil
	},
}
