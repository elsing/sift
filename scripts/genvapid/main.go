// One-shot CLI for setup-env.sh, not part of the server build. Prints
// "<public> <private>" on one line so the shell script can read both with one call.
package main

import (
	"fmt"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func main() {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		panic(err)
	}
	fmt.Println(pub, priv)
}
