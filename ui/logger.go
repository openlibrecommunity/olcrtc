package main

import "fmt"

func log(msg string, args ...interface{}) {
	if len(args) > 0 {
		fmt.Printf(msg+"\n", args...)
	} else {
		fmt.Println(msg)
	}
}
