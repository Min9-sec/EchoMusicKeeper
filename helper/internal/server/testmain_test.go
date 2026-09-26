package server

import (
	"os"
	"testing"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/testtmp"
)

func TestMain(m *testing.M) {
	testtmp.Canonicalize()
	os.Exit(m.Run())
}
