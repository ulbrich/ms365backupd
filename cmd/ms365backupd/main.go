// Command ms365backupd backs up a Microsoft 365 account to an external disk.
package main

import "github.com/ulbrich/ms365backupd/internal/cli"

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() { cli.Execute(version) }
