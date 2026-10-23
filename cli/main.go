// pnat — CLI-клиент платформы PublicNat (NAT-траверсал на базе RatHole).
package main

import (
	"os"

	"github.com/example/nat-platform/cli/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
