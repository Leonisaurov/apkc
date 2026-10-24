package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	start := time.Now()

	if len(os.Args) < 2 {
		printHelp()
		return
	}

	findSDKs()

	switch os.Args[1] {
	case "doctor":
		doctor()
	case "create":
		create()
	case "build":
		build()
	case "run":
		run()
	case "watch":
		watch()
	case "clean":
		clean()
	}

	LogV("apkc", "finished in", time.Since(start))
}

func printHelp() {
	fmt.Print(`
commands:

doctor - check if Android sdk is accessible
create - create a new project
build  - build apk (--inc for an incremental build, --aab for an app bundle,
         --core-library to let d8 dex java.* classes)
run    - build apk and run it on an available device (--inc, --rish)
watch  - rebuild on every change (--inc, --run installs and follows the app)
clean  - delete build/ dir
`)
}
