package main

import (
	"fmt"
	"os"
	"time"
)

// timeIt logs how long a build stage took when the returned function runs. Only
// a stage that did real work is reported: an idle incremental cycle is under
// 100 ms in every one of them and the lines would be noise over watch.
func timeIt(stage string) func() {
	start := time.Now()
	return func() {
		took := time.Since(start)
		if took > 100*time.Millisecond {
			LogD("timing", stage, took.Round(time.Millisecond))
		}
	}
}

func LogI(tag string, args ...interface{}) {
	LogLevel("I", tag, args...)
}

func LogD(tag string, args ...interface{}) {
	LogLevel("D", tag, args...)
}

func LogW(tag string, args ...interface{}) {
	LogLevel("W", tag, args...)
}

func LogE(tag string, args ...interface{}) {
	LogLevel("E", tag, args...)
}

func LogF(tag string, args ...interface{}) {
	LogLevel("E", tag, args...)
	os.Exit(1)
}

func LogV(tag string, args ...interface{}) {
	LogLevel("V", tag, args...)
}

func LogLevel(level string, tag string, args ...interface{}) {
	extra := make([]interface{}, 0)
	switch level {
	case "I":
		extra = append(extra, "\u001b[34m["+tag+"]")
	case "D":
		extra = append(extra, "\u001b[35m["+tag+"]")
	case "W":
		extra = append(extra, "\u001b[33m["+tag+"]")
	case "E":
		extra = append(extra, "\u001b[31m["+tag+"]")
	case "V":
		extra = append(extra, "["+tag+"]")
	}
	extra = append(extra, args...)
	extra = append(extra, "\u001b[0m")
	Log(extra...)
}

func Log(args ...interface{}) {
	fmt.Println(args...)
}
