package ktv

import (
	"io"
	"os"
	"sync"

	"github.com/gookit/color"
	"github.com/kubecolor/kubecolor/config"
	"github.com/kubecolor/kubecolor/kubectl"
	"github.com/kubecolor/kubecolor/printer"
	"github.com/xo/terminfo"
)

// colorize returns r colored by kubecolor as it would color the output of the
// given kubectl command, e.g. "describe pod". Closing the result stops the
// coloring early. Colors are skipped if NO_COLOR is set.
func colorize(cmd []string, dark bool, r io.Reader) (io.ReadCloser, error) {
	if os.Getenv("NO_COLOR") != "" {
		return io.NopCloser(r), nil
	}
	theme, err := kubecolorTheme(dark)
	if err != nil {
		return nil, err
	}

	info := kubectl.InspectSubcommandInfo(cmd, kubectl.DefaultPluginHandler{})
	p := &printer.KubectlOutputColoredPrinter{SubcommandInfo: info, Theme: theme}
	pr, pw := io.Pipe()
	go func() {
		p.Print(r, pw)
		pw.Close()
	}()
	return pr, nil
}

type darkBackground bool

func (d darkBackground) HasDarkBackground() bool { return bool(d) }

// detectColorLevel mirrors kubecolor: detect terminal color support, falling
// back to basic ANSI if it can't be determined. Must precede ComputeCache.
var detectColorLevel = sync.OnceFunc(func() {
	color.DetectColorLevel()
	if color.TermColorLevel() == terminfo.ColorLevelNone && os.Getenv("COLORTERM") == "" {
		color.ForceSetColorLevel(terminfo.ColorLevelBasic)
	}
})

// kubecolorTheme returns kubecolor's default theme for a dark or light
// background, without reading ~/.kube/color.yaml. The theme's derived colors
// are resolved via viper inside kubecolor, so a fresh viper is still needed.
func kubecolorTheme(dark bool) (*config.Theme, error) {
	detectColorLevel()
	cfg, err := config.Unmarshal(config.NewViper(), darkBackground(dark))
	if err != nil {
		return nil, err
	}
	cfg.Theme.ComputeCache()
	return &cfg.Theme, nil
}
