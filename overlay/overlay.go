package overlay

import (
	"fmt"
	"image"
	"log"
	"os"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/shape"
	"github.com/jezek/xgb/xproto"

	"gocrosshair/config"
)

// logWriter is used for warning output from the overlay package.
var logWriter = os.Stderr

// Overlay manages the X11 crosshair overlay window.
type Overlay struct {
	conn      *xgb.Conn
	screen    *xproto.ScreenInfo
	windowID  xproto.Window
	gcID      xproto.Gcontext
	outlineGC xproto.Gcontext
	config    *config.Config
	monitor   Monitor
	centerX   int16
	centerY   int16
	svgImage  *image.RGBA
}

// NewOverlay creates a new crosshair overlay connected to the X server.
func NewOverlay(cfg *config.Config) (*Overlay, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to X server: %w", err)
	}

	// Initialize the shape extension for click-through functionality.
	if err := shape.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to initialize shape extension: %w", err)
	}

	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)

	monitors, err := GetMonitors(conn, screen)
	if err != nil {
		log.Printf("Warning: failed to get monitors: %v, using screen dimensions", err)
		monitors = []Monitor{{
			Name:    "default",
			X:       0,
			Y:       0,
			Width:   screen.WidthInPixels,
			Height:  screen.HeightInPixels,
			Primary: true,
		}}
	}

	monitor := SelectMonitor(monitors, cfg.Position.Monitor)

	centerX := monitor.CenterX() + int16(cfg.Position.OffsetX)
	centerY := monitor.CenterY() + int16(cfg.Position.OffsetY)

	return &Overlay{
		conn:    conn,
		screen:  screen,
		config:  cfg,
		monitor: monitor,
		centerX: centerX,
		centerY: centerY,
	}, nil
}

// Close releases X server resources and closes the connection.
func (o *Overlay) Close() {
	if o.conn != nil {
		o.conn.Close()
	}
}

// createWindow creates the overlay window with override-redirect to bypass WM control.
func (o *Overlay) createWindow() error {
	wid, err := xproto.NewWindowId(o.conn)
	if err != nil {
		return fmt.Errorf("failed to create window ID: %w", err)
	}
	o.windowID = wid

	// Create a full-screen window to ensure we can draw anywhere
	screenWidth := o.screen.WidthInPixels
	screenHeight := o.screen.HeightInPixels

	// Window attributes:
	// - OverrideRedirect: bypass window manager (no decorations, absolute positioning)
	// - BackPixel: background color (will be shaped away)
	// - EventMask: we need exposure events for redrawing
	mask := uint32(xproto.CwBackPixel | xproto.CwOverrideRedirect | xproto.CwEventMask)
	values := []uint32{
		0x000000, // BackPixel: black (will be transparent via shape)
		1,        // OverrideRedirect: true
		xproto.EventMaskExposure | xproto.EventMaskStructureNotify,
	}

	err = xproto.CreateWindowChecked(
		o.conn,
		o.screen.RootDepth,
		o.windowID,
		o.screen.Root,
		0, 0,
		screenWidth,
		screenHeight,
		0,
		xproto.WindowClassInputOutput,
		o.screen.RootVisual,
		mask,
		values,
	).Check()

	if err != nil {
		return fmt.Errorf("failed to create window: %w", err)
	}

	return nil
}

// createGraphicsContext creates graphics contexts for drawing.
func (o *Overlay) createGraphicsContext() error {
	gcid, err := xproto.NewGcontextId(o.conn)
	if err != nil {
		return fmt.Errorf("failed to create GC ID: %w", err)
	}
	o.gcID = gcid

	color := o.config.GetColorUint32()
	mask := uint32(xproto.GcForeground)
	values := []uint32{color}

	if err := xproto.CreateGCChecked(o.conn, o.gcID, xproto.Drawable(o.windowID), mask, values).Check(); err != nil {
		return fmt.Errorf("failed to create GC: %w", err)
	}

	if o.config.Crosshair.OutlineThickness > 0 {
		outlineGC, err := xproto.NewGcontextId(o.conn)
		if err != nil {
			return fmt.Errorf("failed to create outline GC ID: %w", err)
		}
		o.outlineGC = outlineGC

		outlineColor := o.config.GetOutlineColorUint32()
		if err := xproto.CreateGCChecked(o.conn, o.outlineGC, xproto.Drawable(o.windowID), mask, []uint32{outlineColor}).Check(); err != nil {
			return fmt.Errorf("failed to create outline GC: %w", err)
		}
	}

	return nil
}

// preloadSVG renders the SVG once and caches it for shape and draw calls.
func (o *Overlay) preloadSVG() {
	cfg := o.config.Crosshair
	if cfg.Shape != "custom" {
		return
	}
	img, err := RenderSVG(cfg.CustomSVGPath, cfg.Size)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: could not render SVG %q: %v\n", cfg.CustomSVGPath, err)
		return
	}
	o.svgImage = img
}

// applyShape configures the window shape for transparency and click-through.
func (o *Overlay) applyShape() error {
	cfg := o.config.Crosshair

	var boundingRects []xproto.Rectangle

	if cfg.Shape == "custom" {
		if o.svgImage != nil {
			topLeft := o.svgTopLeft()
			boundingRects = svgImageToRects(o.svgImage, topLeft[0], topLeft[1])
		}
	} else {
		shapeRects := GenerateShape(
			cfg.Shape,
			o.centerX,
			o.centerY,
			int16(cfg.Size),
			int16(cfg.Thickness),
			int16(cfg.Gap),
		)

		if cfg.OutlineThickness > 0 {
			outlineRects := GenerateOutline(shapeRects, int16(cfg.OutlineThickness))
			boundingRects = append(boundingRects, outlineRects...)
		}
		boundingRects = append(boundingRects, shapeRects...)
	}

	// Set the BOUNDING shape: defines the visible area of the window
	err := shape.RectanglesChecked(
		o.conn,
		shape.SoSet,
		shape.SkBounding,
		xproto.ClipOrderingUnsorted,
		o.windowID,
		0, 0,
		boundingRects,
	).Check()
	if err != nil {
		return fmt.Errorf("failed to set bounding shape: %w", err)
	}

	// Set the INPUT shape: empty = entire window is click-through
	err = shape.RectanglesChecked(
		o.conn,
		shape.SoSet,
		shape.SkInput,
		xproto.ClipOrderingUnsorted,
		o.windowID,
		0, 0,
		[]xproto.Rectangle{},
	).Check()
	if err != nil {
		return fmt.Errorf("failed to set input shape: %w", err)
	}

	return nil
}

// svgTopLeft returns the screen coordinates of the SVG image's top-left corner.
func (o *Overlay) svgTopLeft() [2]int16 {
	size := int16(o.config.Crosshair.Size)
	return [2]int16{o.centerX - size/2, o.centerY - size/2}
}

// putSVGImage sends the RGBA image to the X window using PutImage (ZPixmap).
// Pixels are converted from RGBA to the X11 BGRX little-endian format.
func (o *Overlay) putSVGImage(img *image.RGBA, dstX, dstY int16) error {
	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()

	data := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			j := (y*w + x) * 4
			pr, pg, pb := img.Pix[i+0], img.Pix[i+1], img.Pix[i+2]
			a := img.Pix[i+3]
			// rasterx writes premultiplied alpha into image.RGBA.
			// Un-premultiply so the true colour reaches the X11 window.
			if a == 255 {
				data[j+0] = pb
				data[j+1] = pg
				data[j+2] = pr
			} else if a > 0 {
				data[j+0] = byte(int(pb) * 255 / int(a))
				data[j+1] = byte(int(pg) * 255 / int(a))
				data[j+2] = byte(int(pr) * 255 / int(a))
			}
			data[j+3] = 0 // padding (X11 ZPixmap, not alpha)
		}
	}

	return xproto.PutImageChecked(
		o.conn,
		xproto.ImageFormatZPixmap,
		xproto.Drawable(o.windowID),
		o.gcID,
		uint16(w), uint16(h),
		dstX, dstY,
		0,
		o.screen.RootDepth,
		data,
	).Check()
}

// drawCrosshair renders the crosshair onto the window.
func (o *Overlay) drawCrosshair() error {
	cfg := o.config.Crosshair

	if cfg.Shape == "custom" {
		if o.svgImage == nil {
			return nil
		}
		topLeft := o.svgTopLeft()
		if err := o.putSVGImage(o.svgImage, topLeft[0], topLeft[1]); err != nil {
			fmt.Fprintf(logWriter, "Warning: failed to draw SVG crosshair: %v\n", err)
		}
		return nil
	}

	shapeRects := GenerateShape(
		cfg.Shape,
		o.centerX,
		o.centerY,
		int16(cfg.Size),
		int16(cfg.Thickness),
		int16(cfg.Gap),
	)

	if cfg.OutlineThickness > 0 && o.outlineGC != 0 {
		outlineRects := GenerateOutline(shapeRects, int16(cfg.OutlineThickness))
		if len(outlineRects) > 0 {
			if err := xproto.PolyFillRectangleChecked(o.conn, xproto.Drawable(o.windowID), o.outlineGC, outlineRects).Check(); err != nil {
				return fmt.Errorf("failed to draw outline: %w", err)
			}
		}
	}

	if len(shapeRects) > 0 {
		if err := xproto.PolyFillRectangleChecked(o.conn, xproto.Drawable(o.windowID), o.gcID, shapeRects).Check(); err != nil {
			return fmt.Errorf("failed to draw crosshair: %w", err)
		}
	}

	return nil
}

// Run initializes and runs the overlay event loop.
func (o *Overlay) Run() error {
	if err := o.createWindow(); err != nil {
		return err
	}

	if err := o.createGraphicsContext(); err != nil {
		return err
	}

	o.preloadSVG()

	if err := o.applyShape(); err != nil {
		return err
	}

	if err := xproto.MapWindowChecked(o.conn, o.windowID).Check(); err != nil {
		return fmt.Errorf("failed to map window: %w", err)
	}

	if err := o.drawCrosshair(); err != nil {
		return err
	}

	log.Printf("Crosshair overlay running on monitor %q at (%d, %d). Press Ctrl+C to exit.",
		o.monitor.Name, o.centerX, o.centerY)

	for {
		ev, err := o.conn.WaitForEvent()
		if err != nil {
			return fmt.Errorf("X11 connection error: %w", err)
		}

		if ev == nil {
			return nil
		}

		switch ev.(type) {
		case xproto.ExposeEvent:
			if err := o.drawCrosshair(); err != nil {
				log.Printf("Warning: failed to redraw crosshair: %v", err)
			}
		}
	}
}

// ListMonitors connects to X server and prints available monitors.
func ListMonitors() error {
	conn, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("failed to connect to X server: %w", err)
	}
	defer conn.Close()

	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)

	monitors, err := GetMonitors(conn, screen)
	if err != nil {
		return fmt.Errorf("failed to get monitors: %w", err)
	}

	PrintMonitors(monitors)
	return nil
}
