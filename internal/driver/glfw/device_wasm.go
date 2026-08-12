//go:build wasm

package glfw

import (
	"regexp"
	"strings"
	"sync/atomic"
	"syscall/js"

	"fyne.io/fyne/v2"
)

var (
	isMobile bool
	isMacOS  bool

	setCursor func(name string)

	blurDummyEntry  func(...any) js.Value
	focusDummyEntry func(...any) js.Value
)

func init() {
	navigator := js.Global().Get("navigator")
	isMobile = regexp.MustCompile("Android|iPhone|iPad|iPod").
		MatchString(navigator.Get("userAgent").String())
	isMacOS = strings.Contains(navigator.Get("platform").String(), "Mac")

	document := js.Global().Get("document")
	style := document.Get("body").Get("style")
	setStyleProperty := style.Get("setProperty").Call("bind", style)
	setCursor = func(name string) {
		setStyleProperty.Invoke("cursor", name)
	}

	dummyEntry := document.Call("getElementById", "dummyEntry")
	if dummyEntry.IsNull() {
		return
	}

	blurDummyEntry = dummyEntry.Get("blur").Call("bind", dummyEntry).Invoke
	focusDummyEntry = dummyEntry.Get("focus").Call("bind", dummyEntry).Invoke

	// #dummyEntry sólo existía para forzar el teclado virtual (foco/blur,
	// arriba): nadie leía lo que se escribía ahí. Los teclados de Android/iOS
	// trabajan por IME y rara vez disparan un keydown con una tecla
	// imprimible (llega "Unidentified"/keyCode 229) — el canal real de texto
	// es el evento `input` del elemento con foco del DOM. Sin este listener,
	// el teclado aparece pero nada de lo tipeado llega al widget enfocado.
	dummyEntry.Call("addEventListener", "input", js.FuncOf(func(_ js.Value, args []js.Value) (result any) {
		// Un panic acá, sin capturar, se lleva puesto todo el runtime de
		// wasm (no sólo este callback) — la app entera queda congelada.
		defer func() {
			if recovered := recover(); recovered != nil {
				fyne.LogError("panic leyendo el evento de #dummyEntry", nil)
			}
		}()

		if len(args) == 0 {
			return nil
		}

		event := args[0]

		// Mientras el IME está componiendo (predicción/autocorrección —
		// el modo por defecto de Gboard e iOS, no un caso raro), el texto
		// es provisional y el navegador lo dibuja y lo administra él
		// mismo. Relayarlo acá duplicaría texto al confirmar (llega de
		// nuevo, completo, en el evento con isComposing=false).
		//
		// isComposing.Bool() panicaría si la propiedad no fuera booleana;
		// chequeamos el tipo primero porque no todos los navegadores
		// garantizan que esté presente en cada evento.
		if isComposing := event.Get("isComposing"); isComposing.Type() == js.TypeBoolean && isComposing.Bool() {
			return nil
		}

		// Todo lo que este callback necesita del evento se lee ACÁ (son
		// operaciones de JS puras, sin tocar Fyne) y se encola como datos
		// planos — nunca se llama a TypedRune/TypedKey desde el callback
		// de JS directamente. El navegador puede disparar estos eventos
		// más rápido de lo que una vuelta completa a fyne.DoAndWait tarda
		// en resolver (tipeo rápido); despachar cada uno desde su propio
		// callback superpone llamadas concurrentes al mismo estado interno
		// de Fyne, que es justo lo que terminó colgando la app. El canal
		// serializa: sólo pumpDummyEntryEvents (una goroutine que nosotros
		// creamos, el caso de uso soportado de fyne.DoAndWait, ver
		// thread.go) los despacha, uno por vez.
		// data.String() sobre null/undefined (deleteContentBackward no
		// trae texto insertado) NO da "" — da un placeholder tipo "<null>"
		// que se insertaría como texto literal si no se filtra acá,
		// mientras el evento sigue siendo un js.Value con el que es
		// seguro operar en este callback.
		text := ""
		if data := event.Get("data"); data.Truthy() {
			text = data.String()
		}

		queueDummyEntryEvent(dummyEntryEvent{
			inputType: event.Get("inputType").String(),
			data:      text,
		})

		// #dummyEntry nunca retiene texto: es un relé, no un campo real.
		// Vaciarlo después de cada evento evita que se acumule algo que el
		// usuario jamás ve (está fuera de pantalla) y simplifica la lectura
		// del próximo evento, que siempre parte de vacío. Es una operación
		// de DOM pura, no hace falta pasar por el canal.
		dummyEntry.Set("value", "")

		return nil
	}))

	go pumpDummyEntryEvents()
}

// activeCanvas es el canvas cuyo widget con foco recibe lo que llega por
// #dummyEntry. Sólo hace falta guardar uno: en wasm, Fyne corre una única
// ventana por proceso. atomic.Pointer porque se escribe desde la goroutine
// principal (connectKeyboard) y se lee desde la del pump — en wasm no hay
// paralelismo real, así que no puede haber corrupción a mitad de escritura,
// pero es una garantía gratis contra reordenar lecturas de un valor viejo.
var activeCanvas atomic.Pointer[glCanvas]

// dummyEntryEvent es la porción de un evento `input` del navegador que
// dispatchDummyEntryEvent necesita, ya leída fuera del callback de JS (ver
// el comentario en init()).
type dummyEntryEvent struct {
	inputType string
	data      string
}

// dummyEntryEvents es el único punto de entrada a Fyne para lo que se
// tipea por #dummyEntry: cada evento del navegador se encola acá, nunca se
// despacha desde el callback de JS que lo generó.
var dummyEntryEvents = make(chan dummyEntryEvent, 64)

func queueDummyEntryEvent(event dummyEntryEvent) {
	select {
	case dummyEntryEvents <- event:
	default:
		// El canal lleno significa tipeo más rápido de lo que se puede
		// despachar — se prioriza no bloquear el callback de JS (bloquearlo
		// congelaría la página igual que el bug que esto reemplaza) antes
		// que garantizar que ni una pulsación se pierda bajo una ráfaga
		// extrema.
		fyne.LogError("cola de #dummyEntry llena, se descartó una pulsación", nil)
	}
}

// pumpDummyEntryEvents es la única goroutine que traduce lo tipeado por
// #dummyEntry a llamadas de Fyne — "una goroutine que nosotros creamos" es
// justo el caso de uso soportado por fyne.DoAndWait (thread.go). Al vaciar
// el canal de a un evento por vez, nunca hay dos despachos en vuelo al
// mismo tiempo, que es lo que un callback de JS por evento no puede
// garantizar bajo tipeo rápido.
func pumpDummyEntryEvents() {
	for event := range dummyEntryEvents {
		dispatchDummyEntryEvent(event)
	}
}

// dispatchDummyEntryEvent traduce un evento ya leído al widget realmente
// enfocado, usando InputEvent.inputType (soportado en los navegadores
// móviles vigentes) para distinguir borrado/salto de línea de texto
// insertado.
func dispatchDummyEntryEvent(event dummyEntryEvent) {
	if activeCanvas.Load() == nil {
		return
	}

	fyne.DoAndWait(func() {
		// El recover tiene que vivir ACÁ adentro, no alrededor del
		// DoAndWait: esta func corre en la goroutine que Fyne usa para
		// drenar su cola principal (runOnMainWithWait, loop.go), no en la
		// que llamó a DoAndWait — un panic ahí se lleva puesto todo el
		// programa sin importar qué recover haya en la goroutine que
		// esperaba el resultado.
		defer func() {
			if recovered := recover(); recovered != nil {
				fyne.LogError("panic despachando un evento de #dummyEntry", nil)
			}
		}()

		switch event.inputType {
		case "deleteContentBackward":
			dispatchTypedKey(fyne.KeyBackspace)
		case "deleteContentForward":
			dispatchTypedKey(fyne.KeyDelete)
		case "insertLineBreak":
			dispatchTypedKey(fyne.KeyReturn)
		default:
			// insertText, insertCompositionText, insertFromComposition,
			// insertReplacementText: todos traen el texto insertado en
			// `data`. Puede ser más de un carácter (autocompletado,
			// predicción, una palabra entera al confirmar composición).
			for _, r := range event.data {
				dispatchTypedRune(r)
			}
		}
	})
}

func dispatchTypedRune(r rune) {
	c := activeCanvas.Load()
	if focused := c.Focused(); focused != nil {
		focused.TypedRune(r)
	} else if c.onTypedRune != nil {
		c.onTypedRune(r)
	}
}

func dispatchTypedKey(name fyne.KeyName) {
	keyEvent := &fyne.KeyEvent{Name: name}
	c := activeCanvas.Load()
	if focused := c.Focused(); focused != nil {
		focused.TypedKey(keyEvent)
	} else if c.onTypedKey != nil {
		c.onTypedKey(keyEvent)
	}
}

func (*glDevice) IsMobile() bool {
	return isMobile
}

func (*glDevice) SystemScaleForWindow(w fyne.Window) float32 {
	// Get the scale information from the web browser directly
	return float32(js.Global().Get("devicePixelRatio").Float())
}

func (*glDevice) hideVirtualKeyboard() {
	if blurDummyEntry == nil {
		return
	}
	blurDummyEntry()
}

func (*glDevice) showVirtualKeyboard() {
	if focusDummyEntry == nil {
		return
	}
	focusDummyEntry()
}

func connectKeyboard(c *glCanvas) {
	activeCanvas.Store(c)
	c.OnFocus = handleKeyboard
	c.OnUnfocus = hideVirtualKeyboard
}

func isMacOSRuntime() bool {
	return isMacOS // Value depends on which OS the browser is running on.
}
