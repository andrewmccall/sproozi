// chatgpt-login is a local administrator helper, never a sandbox command.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/andrewmccall/sproozi/internal/modelauth"
)

func main() {
	path := flag.String("output", ".local/chatgpt-session.json", "Private credential record")
	host := flag.String("host", ".local/chatgpt-host.json", "Stable local host registration")
	model := flag.String("model", "gpt-6.1-sol", "Requested demo model; never automatically substituted")
	check := flag.Bool("check", false, "Validate existing private credentials without a browser or network")
	reuse := flag.Bool("reuse", false, "Reuse a saved session instead of repeating browser consent")
	flag.Parse()
	if *check {
		if _, err := modelauth.LoadFile(*path); err != nil {
			fail(err)
		}
		fmt.Println("Private ChatGPT registration is available")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fmt.Println("Tokens stay private on this host and the trusted gateway, never in the agent sandbox.")
	var session modelauth.Session
	var err error
	if *reuse {
		session, err = modelauth.LoadFile(*path)
		if err == nil && time.Until(session.SavedAt.Add(time.Duration(session.ExpiresIn)*time.Second)) <= time.Minute {
			err = fmt.Errorf("saved login has expired; run bash hack/demo/chatgpt-login.sh --reauthorize")
		}
	} else {
		fmt.Println("Continue with ChatGPT: approve Sproozi's requested plan permissions in your browser.")
		session, err = modelauth.Login(ctx, *path, *host, openBrowser)
	}
	if err != nil {
		fail(err)
	}
	fmt.Println("Checking the requested model; if absent from the catalog, one tiny inference may consume plan tokens.")
	if err = modelauth.CheckModel(ctx, session, *model); err != nil {
		fail(err)
	}
	fmt.Printf("ChatGPT sign-in verified; %s is available. Private credentials saved.\n", *model)
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

func openBrowser(url string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	// Never print the command: returning authorization URLs contain an ID token.
	return exec.Command(command, url).Run()
}
