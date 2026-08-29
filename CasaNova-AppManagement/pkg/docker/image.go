/*
credit: https://github.com/containrrr/watchtower
*/
package docker

import (
	"context"
	"io"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/docker/docker/api/types/image"
	"go.uber.org/zap"
)

func InspectImage(ctx context.Context, imageName string) (*image.InspectResponse, error) {
	cli, err := SharedClient()
	if err != nil {
		return nil, err
	}

	imageInfo, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return nil, err
	}

	return &imageInfo, nil
}

func PullImage(ctx context.Context, imageName string, handleOut func(io.ReadCloser)) error {
	cli, err := SharedClient()
	if err != nil {
		return err
	}

	opts, err := GetPullOptions(imageName)
	if err != nil {
		return err
	}

	out, err := cli.ImagePull(ctx, imageName, opts)
	if err != nil {
		return err
	}
	defer out.Close()

	if handleOut != nil {
		handleOut(out)
	} else {
		if _, err := io.ReadAll(out); err != nil {
			return err
		}
	}

	return nil
}

// HasNewImage reports whether a newer copy of imageName is available
// upstream than the local image whose ID is currentImageID. It MUST
// pull the image from the registry first — the previous version only
// inspected the local image, so the returned ID was always the same
// one we already had on disk and `hasNew` was always false. The
// auto-update job above HasNewImage would then dutifully report "no
// update available" forever and updates effectively never happened.
//
// Pulling here is intentional: the caller (auto_update.go) treats a
// truthy return as authorization to recreate the container against
// the new image, so the new image needs to actually be present
// locally before that recreate runs. We swallow read errors on the
// pull stream the same way PullImage does and re-inspect afterward.
func HasNewImage(ctx context.Context, imageName string, currentImageID string) (bool, string, error) {
	cli, err := SharedClient()
	if err != nil {
		return false, currentImageID, err
	}

	// Pull the latest copy of the image. A failure here is reported
	// back so auto-update can log it and skip this container — we do
	// NOT silently fall back to the local copy, otherwise a registry
	// outage would mask itself as "no update".
	pullOpts, err := GetPullOptions(imageName)
	if err != nil {
		return false, currentImageID, err
	}
	out, err := cli.ImagePull(ctx, imageName, pullOpts)
	if err != nil {
		return false, currentImageID, err
	}
	// Drain the pull stream so the daemon completes the operation
	// before we re-inspect. Without draining, ImageInspectWithRaw
	// below races with the still-streaming pull and may report the
	// pre-pull image ID.
	if _, drainErr := io.Copy(io.Discard, out); drainErr != nil {
		logger.Error("error draining image pull stream",
			zap.String("image", imageName),
			zap.Error(drainErr))
	}
	out.Close()

	newImageInfo, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return false, currentImageID, err
	}

	newImageID := newImageInfo.ID
	if newImageID == currentImageID {
		return false, currentImageID, nil
	}

	return true, newImageID, nil
}
