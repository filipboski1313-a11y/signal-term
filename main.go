package main

import (
	"fmt"
	"os"
	"strings"
)

func printHelp() {
	fmt.Println("usage: signal [-h] [--demo] {link,accounts,send}")
	fmt.Println()
	fmt.Println("Minimalist Signal client for the terminal")
	fmt.Println()
	fmt.Println("commands:")
	fmt.Println("  link                 Link terminal to Signal account via QR code")
	fmt.Println("  accounts             Show linked Signal accounts")
	fmt.Println("  send <to> <msg>      Send a quick message from the command line")
	fmt.Println("  --demo               Launch UI with sample offline demo data")
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			printHelp()
			return
		case "--demo", "demo":
			store, cleanup, err := runDemoStore()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to init demo store: %v\n", err)
				os.Exit(1)
			}
			defer cleanup()
			ui := NewAppUI("+1 555 010 0000", store)
			ui.demoMode = true
			if err := ui.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			return
		case "link":
			if !LinkDeviceInteractive("signal-term") {
				os.Exit(1)
			}
			return
		case "accounts":
			accs := ListAccounts()
			if len(accs) == 0 {
				fmt.Println("No linked accounts. Run: signal link")
			} else {
				for _, a := range accs {
					fmt.Println(a)
				}
			}
			return
		case "send":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: signal send <recipient> <message>")
				os.Exit(1)
			}
			accs := ListAccounts()
			if len(accs) == 0 {
				fmt.Fprintln(os.Stderr, "No linked account. Run: signal link")
				os.Exit(1)
			}
			os.Exit(QuickSend(accs[0], args[1], strings.Join(args[2:], " ")))
		}
	}

	accs := ListAccounts()
	if len(accs) == 0 {
		fmt.Println("No linked Signal account found.")
		if !LinkDeviceInteractive("signal-term") {
			os.Exit(1)
		}
		accs = ListAccounts()
		if len(accs) == 0 {
			fmt.Fprintln(os.Stderr, "Still no account found after linking.")
			os.Exit(1)
		}
	}

	store, err := NewStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open local store: %v\n", err)
		os.Exit(1)
	}

	ui := NewAppUI(accs[0], store)
	if err := ui.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
