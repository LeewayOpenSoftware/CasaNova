package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/jsonmessage"
	"go.uber.org/zap"
)

// Check if image exists
func (ds *dockerService) IsExistImage(imageName string) bool {
	cli, err := docker.SharedClient()
	if err != nil {
		return false
	}
	filter := filters.NewArgs()
	filter.Add("reference", imageName)

	// Wave 27: bound the docker call. context.Background() meant a hung
	// dockerd left this goroutine pinned forever. IsExistImage is on
	// the hot path of every install/update preflight, so a stuck call
	// here cascades into "compose UI freezes for everyone".
	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := cli.ImageList(dctx, image.ListOptions{Filters: filter})

	if err == nil && len(list) > 0 {
		return true
	}

	return false
}

// Install image
func (ds *dockerService) PullImage(ctx context.Context, imageName string) error {
	go PublishEventWrapper(ctx, common.EventTypeImagePullBegin, map[string]string{
		common.PropertyTypeImageName.Name: imageName,
	})

	defer PublishEventWrapper(ctx, common.EventTypeImagePullEnd, map[string]string{
		common.PropertyTypeImageName.Name: imageName,
	})

	if err := docker.PullImage(ctx, imageName, func(out io.ReadCloser) {
		pullImageProgress(ctx, out, "INSTALL", 1, 1)
	}); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
			common.PropertyTypeImageName.Name: imageName,
			common.PropertyTypeMessage.Name:   err.Error(),
		})
		return err
	}

	return nil
}

// Try to pull latest image.
//
// It returns `true` if the image is updated.
func (ds *dockerService) PullLatestImage(ctx context.Context, imageName string) (bool, error) {
	isImageUpdated := false

	go PublishEventWrapper(ctx, common.EventTypeImagePullBegin, map[string]string{
		common.PropertyTypeImageName.Name: imageName,
	})

	defer PublishEventWrapper(ctx, common.EventTypeImagePullEnd, map[string]string{
		common.PropertyTypeImageName.Name: imageName,

		// update image update information in the defer func below, instead of here.
		// this because PublishEventWrapper will retrieve the information from context and include all properties in the event.
		//
		// common.PropertyTypeImageUpdated.Name: fmt.Sprint(isImageUpdated),  // <- no need to do it here.
	})

	defer func() {
		// write image updated information as a property back to context, so both current func and external caller can see it
		common.SetProperty(ctx, common.PropertyTypeImageUpdated.Name, fmt.Sprint(isImageUpdated)) // <- instead, do it here.
	}()

	if strings.HasPrefix(imageName, "sha256:") {
		message := "container uses a pinned image, and cannot be updated"
		go PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
			common.PropertyTypeImageName.Name: imageName,
			common.PropertyTypeMessage.Name:   message,
		})

		return false, errors.New(message)
	}

	imageInfo1, err := docker.InspectImage(ctx, imageName)
	if err != nil {
		go PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
			common.PropertyTypeImageName.Name: imageName,
			common.PropertyTypeMessage.Name:   err.Error(),
		})
		return false, err
	}

	if match, err := docker.CompareDigest(ctx, imageName, imageInfo1.RepoDigests); err != nil {
		// do nothing
	} else if match {
		return false, nil
	}

	if err = docker.PullImage(ctx, imageName, func(out io.ReadCloser) {
		pullImageProgress(ctx, out, "UPDATE", 1, 1)
	}); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
			common.PropertyTypeImageName.Name: imageName,
			common.PropertyTypeMessage.Name:   err.Error(),
		})
		return false, err
	}

	imageInfo2, err := docker.InspectImage(ctx, imageName)
	if err != nil {
		go PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
			common.PropertyTypeImageName.Name: imageName,
			common.PropertyTypeMessage.Name:   err.Error(),
		})
		return false, err
	}

	isImageUpdated = imageInfo1.ID != imageInfo2.ID
	return isImageUpdated, nil
}

// Remove image
func (ds *dockerService) RemoveImage(name string) error {
	cli, err := docker.SharedClient()
	if err != nil {
		return err
	}
	// Wave 27: timeouts on both the list and the actual remove. The
	// list call could enumerate hundreds of images on a busy host;
	// give it a generous 60s. The remove itself should be quick on a
	// healthy daemon, but bound it at 2 minutes so a stuck untag
	// (busy children, missing references) eventually surfaces as a
	// real error instead of an indefinite hang.
	listCtx, listCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer listCancel()
	imageList, err := cli.ImageList(listCtx, image.ListOptions{})
	if err != nil {
		return err
	}

	imageID := ""

Loop:
	for _, ig := range imageList {
		for _, i := range ig.RepoTags {
			if i == name {
				imageID = ig.ID
				break Loop
			}
		}
	}

	rmCtx, rmCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer rmCancel()
	_, err = cli.ImageRemove(rmCtx, imageID, image.RemoveOptions{})
	return err
}

type StatusType string

const (
	Pull         StatusType = "Pulling fs layer"
	PullComplete StatusType = "Pull complete"
)

type ProgressDetail struct {
	Current int64 `json:"current"`
	Total   int64 `json:"total"`
}

type PullOut struct {
	Status         StatusType     `json:"status"`
	ProgressDetail ProgressDetail `json:"progressDetail"`
	Id             string         `json:"id"`
}

type Throttler struct {
	InvokeInterval time.Duration
	LastInvokeTime time.Time
}

func NewThrottler(interval time.Duration) *Throttler {
	return &Throttler{
		InvokeInterval: interval,
	}
}

func (t *Throttler) ThrottleFunc(f func()) {
	if time.Since(t.LastInvokeTime) >= t.InvokeInterval {
		f()
		t.LastInvokeTime = time.Now()
	}
}

func pullImageProgress(ctx context.Context, out io.ReadCloser, notificationType string, totalImageNum int, currentImage int) {
	// NOTE: do NOT close `out` here. docker.PullImage already calls
	// `defer out.Close()` at pkg/docker/image.go after this callback
	// returns, so closing it twice would race the second close against
	// the docker client and (depending on the underlying reader)
	// either return an "already closed" error or panic. The earlier
	// "leak" theory was wrong: the lifetime is owned by PullImage.
	layerNum := 0
	completedLayerNum := 0
	decoder := json.NewDecoder(out)
	if decoder == nil {
		logger.Error("failed to create json decoder")
		return
	}

	throttler := NewThrottler(500 * time.Millisecond)

	for decoder.More() {
		var message jsonmessage.JSONMessage
		if err := decoder.Decode(&message); err != nil {
			logger.Error("failed to decode json message", zap.Error(err))
			continue
		}

		switch message.Status {
		// pull a new layer
		case string(Pull):
			layerNum++
		// pull a layer complete
		case string(PullComplete):
			completedLayerNum++
		}

		// layer progress
		if layerNum == 0 {
			continue
		}
		completedFraction := float32(completedLayerNum) / float32(layerNum)

		// image progress
		currentImageFraction := float32(currentImage) / float32(totalImageNum)
		progress := completedFraction * currentImageFraction * 100

		// reduce the event send frequency
		throttler.ThrottleFunc(func() {
			go func(progress int) {
				// ensure progress is in [0, 100]
				if progress < 0 {
					progress = 0
				}
				if progress > 100 {
					progress = 100
				}

				PublishEventWrapper(ctx, common.EventTypeAppInstallProgress, map[string]string{
					common.PropertyTypeAppProgress.Name: fmt.Sprintf("%d", progress),
				})
			}(int(progress))
		})
	}
}
