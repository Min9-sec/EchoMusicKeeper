package proxy

import (
	"context"
	"io"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

type downloadCopyFunc func(context.Context, io.Reader, int64, *security.Directory, model.TrackInfo) (string, error)

type downloadCleanupFunc func(*security.Directory, string) error
