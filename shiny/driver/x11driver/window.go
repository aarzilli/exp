// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package x11driver

// TODO: implement a back buffer.

import (
	"image"
	"image/color"
	"image/draw"
	"sync"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/render"
	"github.com/BurntSushi/xgb/xproto"

	"golang.org/x/exp/shiny/driver/internal/drawer"
	"golang.org/x/exp/shiny/driver/internal/event"
	"golang.org/x/exp/shiny/driver/internal/lifecycler"
	"golang.org/x/exp/shiny/driver/internal/x11key"
	"golang.org/x/exp/shiny/screen"
	"golang.org/x/image/math/f64"
	"golang.org/x/mobile/event/key"
	"golang.org/x/mobile/event/mouse"
	"golang.org/x/mobile/event/paint"
	"golang.org/x/mobile/event/size"
	"golang.org/x/mobile/geom"
)

type windowImpl struct {
	s *screenImpl

	xw xproto.Window
	xg xproto.Gcontext
	xp render.Picture

	event.Deque
	xevents chan xgb.Event

	// This next group of variables are mutable, but are only modified in the
	// screenImpl.run goroutine.
	width, height int

	lifecycler lifecycler.State

	mu       sync.Mutex
	released bool
}

func (w *windowImpl) Release() {
	w.mu.Lock()
	released := w.released
	w.released = true
	w.mu.Unlock()

	// TODO: call w.lifecycler.SetDead and w.lifecycler.SendEvent, a la
	// handling atomWMDeleteWindow?

	if released {
		return
	}
	render.FreePicture(w.s.xc, w.xp)
	xproto.FreeGC(w.s.xc, w.xg)
	xproto.DestroyWindow(w.s.xc, w.xw)
}

func (w *windowImpl) Upload(dp image.Point, src screen.Buffer, sr image.Rectangle) {
	src.(*bufferImpl).upload(xproto.Drawable(w.xw), w.xg, w.s.xsi.RootDepth, dp, sr)
}

func (w *windowImpl) Fill(dr image.Rectangle, src color.Color, op draw.Op) {
	fill(w.s.xc, w.xp, dr, src, op)
}

func (w *windowImpl) DrawUniform(src2dst f64.Aff3, src color.Color, sr image.Rectangle, op draw.Op, opts *screen.DrawOptions) {
	w.s.drawUniform(w.xp, &src2dst, src, sr, op, opts)
}

func (w *windowImpl) Draw(src2dst f64.Aff3, src screen.Texture, sr image.Rectangle, op draw.Op, opts *screen.DrawOptions) {
	src.(*textureImpl).draw(w.xp, &src2dst, sr, op, opts)
}

func (w *windowImpl) Copy(dp image.Point, src screen.Texture, sr image.Rectangle, op draw.Op, opts *screen.DrawOptions) {
	drawer.Copy(w, dp, src, sr, op, opts)
}

func (w *windowImpl) Scale(dr image.Rectangle, src screen.Texture, sr image.Rectangle, op draw.Op, opts *screen.DrawOptions) {
	drawer.Scale(w, dr, src, sr, op, opts)
}

func (w *windowImpl) Publish() screen.PublishResult {
	// TODO: implement a back buffer, and copy or flip that here to the front
	// buffer.

	// This sync isn't needed to flush the outgoing X11 requests. Instead, it
	// acts as a form of flow control. Outgoing requests can be quite small on
	// the wire, e.g. draw this texture ID (an integer) to this rectangle (four
	// more integers), but much more expensive on the server (blending a
	// million source and destination pixels). Without this sync, the Go X11
	// client could easily end up sending work at a faster rate than the X11
	// server can serve.
	w.s.xc.Sync()

	return screen.PublishResult{}
}

func (w *windowImpl) SetTitle(title string) error {
	buf := []byte(title)
	return xproto.ChangePropertyChecked(w.s.xc, xproto.PropModeReplace, w.xw, w.s.atomNetWMName, w.s.atomUTF8String, 8, uint32(len(buf)), buf).Check()
}

func (w *windowImpl) SetCursor(cursor screen.Cursor) error {
	if cursorId, ok := w.s.cursorCache[cursor]; ok {
		xproto.ChangeWindowAttributes(w.s.xc, w.xw, xproto.CwCursor, []uint32{uint32(cursorId)})
	}
	return nil
}

func (w *windowImpl) WarpMouse(p image.Point) error {
	gifr, err := xproto.GetInputFocus(w.s.xc).Reply()
	if err != nil {
		return err
	}

	if gifr.Focus != w.xw {
		return nil
	}

	screen := xproto.Setup(w.s.xc).DefaultScreen(w.s.xc)
	tp, err := w.translateToScreen(screen, p)
	if err != nil {
		return err
	}
	wpc := xproto.WarpPointerChecked(w.s.xc, 0, screen.Root, 0, 0, 0, 0, int16(tp.X), int16(tp.Y))
	return wpc.Check()
}

func (w *windowImpl) translateToScreen(screen *xproto.ScreenInfo, p image.Point) (r image.Point, err error) {
	tcc := xproto.TranslateCoordinates(w.s.xc, w.xw, screen.Root, int16(p.X), int16(p.Y))
	tcr, err := tcc.Reply()
	if err != nil {
		return
	}
	r.X = int(tcr.DstX)
	r.Y = int(tcr.DstY)
	return
}

func (w *windowImpl) Raise() error {
	screen := xproto.Setup(w.s.xc).DefaultScreen(w.s.xc)

	ev := xproto.ClientMessageEvent{
		Format: 32,
		Window: w.xw,
		Type:   w.s.atomNetActiveWindow,
		Data: xproto.ClientMessageDataUnionData32New([]uint32{
			0,
			uint32(xproto.TimeCurrentTime),
			uint32(0),
			0,
			0,
		}),
	}

	xproto.SendEventChecked(w.s.xc, false, screen.Root, xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(ev.Bytes())).Check()

	xproto.MapWindowChecked(w.s.xc, w.xw).Check()

	return nil
}

func (w *windowImpl) handleConfigureNotify(ev xproto.ConfigureNotifyEvent) {
	// TODO: does the order of these lifecycle and size events matter? Should
	// they really be a single, atomic event?
	w.lifecycler.SetVisible((int(ev.X)+int(ev.Width)) > 0 && (int(ev.Y)+int(ev.Height)) > 0)
	w.lifecycler.SendEvent(w, nil)

	newWidth, newHeight := int(ev.Width), int(ev.Height)
	if w.width == newWidth && w.height == newHeight {
		return
	}
	w.width, w.height = newWidth, newHeight
	w.Send(size.Event{
		WidthPx:     newWidth,
		HeightPx:    newHeight,
		WidthPt:     geom.Pt(newWidth),
		HeightPt:    geom.Pt(newHeight),
		PixelsPerPt: w.s.pixelsPerPt,
	})
}

func (w *windowImpl) handleExpose() {
	w.Send(paint.Event{})
}

func (w *windowImpl) handleKey(detail xproto.Keycode, state uint16, dir key.Direction) {
	r, c := w.s.keysyms.Lookup(uint8(detail), state)
	w.Send(key.Event{
		Rune:      r,
		Code:      c,
		Modifiers: x11key.KeyModifiers(state),
		Direction: dir,
	})
}

func (w *windowImpl) handleMouse(x, y int16, b xproto.Button, state uint16, dir mouse.Direction) {
	// TODO: should a mouse.Event have a separate MouseModifiers field, for
	// which buttons are pressed during a mouse move?
	btn := mouse.Button(b)
	switch btn {
	case 4:
		btn = mouse.ButtonWheelUp
	case 5:
		btn = mouse.ButtonWheelDown
	case 6:
		btn = mouse.ButtonWheelLeft
	case 7:
		btn = mouse.ButtonWheelRight
	}
	if btn.IsWheel() {
		if dir != mouse.DirPress {
			return
		}
		dir = mouse.DirStep
	}
	w.Send(mouse.Event{
		X:         float32(x),
		Y:         float32(y),
		Button:    btn,
		Modifiers: x11key.KeyModifiers(state),
		Direction: dir,
	})
}

func (w *windowImpl) AbsolutePosition() (int, int) {
	translateReply, err := xproto.TranslateCoordinates(w.s.xc, w.xw, w.s.xsi.Root, 0, 0).Reply()
	if err == nil {
		return int(translateReply.DstX), int(translateReply.DstY)
	}
	return 0, 0
}