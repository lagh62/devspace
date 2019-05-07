package image

import (
	"bytes"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/devspace-cloud/devspace/pkg/devspace/config/configutil"
	"github.com/devspace-cloud/devspace/pkg/devspace/config/generated"
	logpkg "github.com/devspace-cloud/devspace/pkg/util/log"
	"github.com/devspace-cloud/devspace/pkg/util/randutil"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// DefaultDockerfilePath is the default dockerfile path to use
const DefaultDockerfilePath = "./Dockerfile"

// DefaultContextPath is the default context path to use
const DefaultContextPath = "./"

type imageNameAndTag struct {
	imageConfigName string
	imageName       string
	imageTag        string
}

// BuildAll builds all images
func BuildAll(client kubernetes.Interface, isDev, forceRebuild, sequential bool, log logpkg.Logger) (map[string]string, error) {
	var (
		config      = configutil.GetConfig()
		builtImages = make(map[string]string)

		// Parallel build
		errChan   = make(chan error)
		cacheChan = make(chan imageNameAndTag)
	)

	// Build not in parallel when we only have one image to build
	if sequential == false && len(*config.Images) <= 1 {
		sequential = true
	}

	generatedConfig, err := generated.LoadConfig()
	if err != nil {
		return nil, errors.Wrap(err, "load generated config")
	}

	// Update config
	cache := generatedConfig.GetActive()

	imagesToBuild := 0
	for imageConfigName, imageConf := range *config.Images {
		if imageConf.Build != nil && imageConf.Build.Disabled != nil && *imageConf.Build.Disabled == true {
			log.Infof("Skipping building image %s", imageConfigName)
			continue
		}

		// This is necessary for parallel build otherwise we would override the image conf pointer during the loop
		cImageConf := *imageConf

		// Create new builder
		builder := newBuilderConfig(client, imageConfigName, &cImageConf, isDev)

		// Check if rebuild is needed
		needRebuild, err := builder.shouldRebuild(cache)
		if err != nil {
			return nil, fmt.Errorf("Error during shouldRebuild check: %v", err)
		}
		if forceRebuild == false && needRebuild == false {
			log.Infof("Skip building image '%s'", imageConfigName)
			continue
		}

		// Get image tag
		imageTag, err := randutil.GenerateRandomString(7)
		if err != nil {
			return nil, fmt.Errorf("Image building failed: %v", err)
		}
		if imageConf.Tag != nil {
			imageTag = *imageConf.Tag
		}

		if sequential {
			// Build the image
			err = builder.Build(imageTag, log)
			if err != nil {
				return nil, err
			}

			// Update cache
			imageCache := cache.GetImageCache(imageConfigName)
			imageCache.ImageName = builder.imageName
			imageCache.Tag = imageTag

			// Track built images
			builtImages[builder.imageName] = imageTag
		} else {
			imagesToBuild++
			go func() {
				// Create a string log
				buff := &bytes.Buffer{}
				streamLog := logpkg.NewStreamLogger(buff, logrus.InfoLevel)

				// Build the image
				err := builder.Build(imageTag, streamLog)
				if err != nil {
					errChan <- fmt.Errorf("Error building image %s:%s: %s %v", builder.imageName, imageTag, buff.String(), err)
					return
				}

				// Send the reponse
				cacheChan <- imageNameAndTag{
					imageConfigName: builder.imageConfigName,
					imageName:       builder.imageName,
					imageTag:        imageTag,
				}
			}()
		}
	}

	if sequential == false && imagesToBuild > 0 {
		defer log.StopWait()

		for imagesToBuild > 0 {
			log.StartWait(fmt.Sprintf("Building %d images...", imagesToBuild))

			select {
			case err := <-errChan:
				return nil, err
			case done := <-cacheChan:
				imagesToBuild--
				log.Donef("Done building image %s (%s:%s)", done.imageConfigName, done.imageName, done.imageTag)

				// Update cache
				imageCache := cache.GetImageCache(done.imageConfigName)
				imageCache.ImageName = done.imageName
				imageCache.Tag = done.imageTag

				// Track built images
				builtImages[done.imageName] = done.imageTag
			}
		}
	}

	return builtImages, nil
}
