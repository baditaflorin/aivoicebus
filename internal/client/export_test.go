package client

import "github.com/baditaflorin/aivoicebus/internal/sound"

// ListenForSounds swaps the sound package for a recorder while a test runs: on says whether sounds
// are on, and every sound the client asks for is appended to the returned log (urgent ones with a "!").
func ListenForSounds(on bool) (heard *[]string, restore func()) {
	oldOn, oldPlay := soundsOn, playSound
	heard = new([]string)
	soundsOn = func() bool { return on }
	playSound = func(e sound.Event, urgent bool) {
		if !on {
			return
		}
		name := string(e)
		if urgent {
			name += "!"
		}
		*heard = append(*heard, name)
	}
	return heard, func() { soundsOn, playSound = oldOn, oldPlay }
}
