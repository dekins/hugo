// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resources

import (
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/png"
	"os"
	"strings"
	"sync"

	"github.com/gohugoio/hugo/resources/images/exif"

	"github.com/gohugoio/hugo/resources/internal"

	"github.com/gohugoio/hugo/resources/resource"

	_errors "github.com/pkg/errors"

	"github.com/disintegration/gift"
	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/resources/images"

	// Blind import for image.Decode

	// Blind import for image.Decode
	_ "golang.org/x/image/webp"
)

var (
	_ resource.Image  = (*imageResource)(nil)
	_ resource.Source = (*imageResource)(nil)
	_ resource.Cloner = (*imageResource)(nil)
)

// ImageResource represents an image resource.
type imageResource struct {
	*images.Image

	// When a image is processed in a chain, this holds the reference to the
	// original (first).
	root *imageResource

	exifInit    sync.Once
	exifInitErr error
	exif        *exif.Exif

	baseResource
}

func (i *imageResource) Exif() (*exif.Exif, error) {
	return i.root.getExif()
}

func (i *imageResource) getExif() (*exif.Exif, error) {

	i.exifInit.Do(func() {
		supportsExif := i.Format == images.JPEG || i.Format == images.TIFF
		if !supportsExif {
			return
		}

		f, err := i.root.ReadSeekCloser()
		if err != nil {
			i.exifInitErr = err
			return
		}
		defer f.Close()

		x, err := i.getSpec().imaging.DecodeExif(f)
		if err != nil {
			i.exifInitErr = err
			return
		}

		i.exif = x

	})

	return i.exif, i.exifInitErr
}

func (i *imageResource) Clone() resource.Resource {
	gr := i.baseResource.Clone().(baseResource)
	return &imageResource{
		root:         i.root,
		Image:        i.WithSpec(gr),
		baseResource: gr,
	}
}

func (i *imageResource) cloneWithUpdates(u *transformationUpdate) (baseResource, error) {
	base, err := i.baseResource.cloneWithUpdates(u)
	if err != nil {
		return nil, err
	}

	var img *images.Image

	if u.isContenChanged() {
		img = i.WithSpec(base)
	} else {
		img = i.Image
	}

	return &imageResource{
		root:         i.root,
		Image:        img,
		baseResource: base,
	}, nil
}

// Resize resizes the image to the specified width and height using the specified resampling
// filter and returns the transformed image. If one of width or height is 0, the image aspect
// ratio is preserved.
func (i *imageResource) Resize(spec string) (resource.Image, error) {
	conf, err := i.decodeImageConfig("resize", spec)
	if err != nil {
		return nil, err
	}

	return i.doWithImageConfig(conf, func(src image.Image) (image.Image, error) {
		return i.Proc.ApplyFiltersFromConfig(src, conf)
	})
}

// Fit scales down the image using the specified resample filter to fit the specified
// maximum width and height.
func (i *imageResource) Fit(spec string) (resource.Image, error) {
	conf, err := i.decodeImageConfig("fit", spec)
	if err != nil {
		return nil, err
	}

	return i.doWithImageConfig(conf, func(src image.Image) (image.Image, error) {
		return i.Proc.ApplyFiltersFromConfig(src, conf)
	})
}

// Fill scales the image to the smallest possible size that will cover the specified dimensions,
// crops the resized image to the specified dimensions using the given anchor point.
// Space delimited config: 200x300 TopLeft
func (i *imageResource) Fill(spec string) (resource.Image, error) {
	conf, err := i.decodeImageConfig("fill", spec)
	if err != nil {
		return nil, err
	}

	return i.doWithImageConfig(conf, func(src image.Image) (image.Image, error) {
		return i.Proc.ApplyFiltersFromConfig(src, conf)
	})
}

func (i *imageResource) Filter(filters ...gift.Filter) (resource.Image, error) {
	conf := i.Proc.GetDefaultImageConfig("filter")
	conf.Key = internal.HashString(filters)

	return i.doWithImageConfig(conf, func(src image.Image) (image.Image, error) {
		return i.Proc.Filter(src, filters...)
	})
}

func (i *imageResource) isJPEG() bool {
	name := strings.ToLower(i.getResourcePaths().relTargetDirFile.file)
	return strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg")
}

// Serialize image processing. The imaging library spins up its own set of Go routines,
// so there is not much to gain from adding more load to the mix. That
// can even have negative effect in low resource scenarios.
// Note that this only effects the non-cached scenario. Once the processed
// image is written to disk, everything is fast, fast fast.
const imageProcWorkers = 1

var imageProcSem = make(chan bool, imageProcWorkers)

func (i *imageResource) doWithImageConfig(conf images.ImageConfig, f func(src image.Image) (image.Image, error)) (resource.Image, error) {
	return i.getSpec().imageCache.getOrCreate(i, conf, func() (*imageResource, image.Image, error) {
		imageProcSem <- true
		defer func() {
			<-imageProcSem
		}()

		errOp := conf.Action
		errPath := i.getSourceFilename()

		src, err := i.decodeSource()
		if err != nil {
			return nil, nil, &os.PathError{Op: errOp, Path: errPath, Err: err}
		}

		converted, err := f(src)
		if err != nil {
			return nil, nil, &os.PathError{Op: errOp, Path: errPath, Err: err}
		}

		if i.Format == images.PNG {
			// Apply the colour palette from the source
			if paletted, ok := src.(*image.Paletted); ok {
				tmp := image.NewPaletted(converted.Bounds(), paletted.Palette)
				draw.FloydSteinberg.Draw(tmp, tmp.Bounds(), converted, converted.Bounds().Min)
				converted = tmp
			}
		}

		ci := i.clone(converted)
		ci.setBasePath(conf)

		return ci, converted, nil
	})
}

func (i *imageResource) decodeImageConfig(action, spec string) (images.ImageConfig, error) {
	conf, err := images.DecodeImageConfig(action, spec, i.Proc.Cfg)
	if err != nil {
		return conf, err
	}

	iconf := i.Proc.Cfg

	if conf.Quality <= 0 && i.isJPEG() {
		// We need a quality setting for all JPEGs
		conf.Quality = iconf.Quality
	}

	return conf, nil
}

func (i *imageResource) decodeSource() (image.Image, error) {
	f, err := i.ReadSeekCloser()
	if err != nil {
		return nil, _errors.Wrap(err, "failed to open image for decode")
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func (i *imageResource) clone(img image.Image) *imageResource {
	spec := i.baseResource.Clone().(baseResource)

	var image *images.Image
	if img != nil {
		image = i.WithImage(img)
	} else {
		image = i.WithSpec(spec)
	}

	return &imageResource{
		Image:        image,
		root:         i.root,
		baseResource: spec,
	}
}

func (i *imageResource) setBasePath(conf images.ImageConfig) {
	i.getResourcePaths().relTargetDirFile = i.relTargetPathFromConfig(conf)
}

func (i *imageResource) relTargetPathFromConfig(conf images.ImageConfig) dirFile {
	p1, p2 := helpers.FileAndExt(i.getResourcePaths().relTargetDirFile.file)
	if conf.Action == "trace" {
		p2 = ".svg"
	}

	h, _ := i.hash()
	idStr := fmt.Sprintf("_hu%s_%d", h, i.size())

	// Do not change for no good reason.
	const md5Threshold = 100

	key := conf.GetKey(i.Format)

	// It is useful to have the key in clear text, but when nesting transforms, it
	// can easily be too long to read, and maybe even too long
	// for the different OSes to handle.
	if len(p1)+len(idStr)+len(p2) > md5Threshold {
		key = helpers.MD5String(p1 + key + p2)
		huIdx := strings.Index(p1, "_hu")
		if huIdx != -1 {
			p1 = p1[:huIdx]
		} else {
			// This started out as a very long file name. Making it even longer
			// could melt ice in the Arctic.
			p1 = ""
		}
	} else if strings.Contains(p1, idStr) {
		// On scaling an already scaled image, we get the file info from the original.
		// Repeating the same info in the filename makes it stuttery for no good reason.
		idStr = ""
	}

	return dirFile{
		dir:  i.getResourcePaths().relTargetDirFile.dir,
		file: fmt.Sprintf("%s%s_%s%s", p1, idStr, key, p2),
	}
}
