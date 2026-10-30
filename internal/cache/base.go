package cache

import (
	"os"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
)

var (
	ValidDuration     = 1 * time.Minute
	cleanTaskInterval = ValidDuration / 2

	lastClean                     time.Time
	skippedCleanWithCanvasRefresh = false

	// testing purpose only
	timeNow = time.Now
)

func init() {
	if t, err := time.ParseDuration(os.Getenv("FYNE_CACHE")); err == nil {
		ValidDuration = t
		cleanTaskInterval = ValidDuration / 2
	}
}

// Clean run cache clean task, it should be called on paint events.
func Clean(canvasRefreshed bool) {
	now := timeNow()
	// do not run clean task too fast
	if now.Sub(lastClean) < 10*time.Second {
		if canvasRefreshed {
			skippedCleanWithCanvasRefresh = true
		}
		return
	}
	if skippedCleanWithCanvasRefresh {
		skippedCleanWithCanvasRefresh = false
		canvasRefreshed = true
	}
	if !canvasRefreshed && now.Sub(lastClean) < cleanTaskInterval {
		return
	}
	destroyExpiredSvgs(now)
	destroyExpiredFontMetrics(now)
	if canvasRefreshed {
		// Destroy renderers on canvas refresh to avoid flickering screen.
		destroyExpiredRenderers(now)
		// canvases cache should be invalidated only on canvas refresh, otherwise there wouldn't
		// be a way to recover them later
		destroyExpiredCanvases(now)
	}
	lastClean = timeNow()
}

// CleanCanvas performs a complete remove of all the objects that belong to the specified
// canvas. Usually used to free all objects from a closing windows.
func CleanCanvas(canvas fyne.Canvas) {
	canvases.Range(func(obj fyne.CanvasObject, cinfo *canvasInfo) bool {
		if cinfo.canvas != canvas {
			return true
		}

		canvases.Delete(obj)

		wid, ok := obj.(fyne.Widget)
		if !ok {
			return true
		}
		rinfo, ok := renderers.LoadAndDelete(wid)
		if !ok {
			return true
		}
		rinfo.renderer.Destroy()
		overrides.Delete(wid)
		return true
	})
}

// ResetThemeCaches clears all the svg and text size cache maps
func ResetThemeCaches() {
	svgs.Clear()
	fontSizeCache.Clear()
}

// destroyExpiredCanvases deletes objects from the canvases cache.
func destroyExpiredCanvases(now time.Time) {
	canvases.Range(func(obj fyne.CanvasObject, cinfo *canvasInfo) bool {
		if cinfo.isExpired(now) {
			canvases.Delete(obj)
		}
		return true
	})
}

// destroyExpiredRenderers deletes the renderer from the cache and calls
// renderer.Destroy()
func destroyExpiredRenderers(now time.Time) {
	renderers.Range(func(wid fyne.Widget, rinfo *rendererInfo) bool {
		if rinfo.isExpired(now) {
			rinfo.renderer.Destroy()
			overrides.Delete(wid)
			renderers.Delete(wid)
		}
		return true
	})
}

type expiringCache struct {
	expires time.Time
}

// isExpired check if the cache data is expired.
func (c *expiringCache) isExpired(now time.Time) bool {
	return c.expires.Before(now)
}

// setAlive updates expiration time.
func (c *expiringCache) setAlive() {
	c.expires = aliveNow().Add(ValidDuration)
}

// tickNanos is the time of the driver's last loop tick (see Tick), or 0 if the
// driver does not tick.
var tickNanos atomic.Int64

// Tick records the current time for setAlive. Drivers call it on every loop
// tick, so setAlive reads one clock per tick instead of one per object: a paint
// walks every visible object and refreshes its cache entry, and on systems
// where the clock is slow to read (e.g. Linux falling back to the HPET
// clocksource after the TSC is marked unstable, ~3µs per time.Now) that alone
// made each frame cost milliseconds. Expiry is measured in minutes, so a time
// at most one tick old changes nothing.
func Tick() {
	tickNanos.Store(timeNow().UnixNano())
}

// aliveNow is the time setAlive uses: the last tick when the driver ticks, which
// is at most one tick old, or the real time otherwise.
func aliveNow() time.Time {
	if nanos := tickNanos.Load(); nanos != 0 {
		return time.Unix(0, nanos)
	}

	return timeNow()
}
