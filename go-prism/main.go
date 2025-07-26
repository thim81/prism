package main

import (
	"flag"
	"log"

	"prism-go/server"
)

func main() {
	spec := flag.String("spec", "", "path to openapi spec")
	port := flag.Int("port", 4010, "port to serve")
	dynamic := flag.Bool("dynamic", false, "dynamic responses")
	ignoreExamples := flag.Bool("ignoreExamples", false, "ignore examples in static mode")
	fillProps := flag.Bool("json-schema-faker-fillProperties", true, "fill optional properties")
	watch := flag.Bool("watch", true, "watch spec for changes")

	flag.Parse()

	if *spec == "" {
		log.Fatal("spec file is required")
	}

	opts := server.Options{
		Port:           *port,
		Dynamic:        *dynamic,
		IgnoreExamples: *ignoreExamples,
		FillProperties: *fillProps,
		Watch:          *watch,
	}

	s, err := server.New(*spec, opts)
	if err != nil {
		log.Fatal(err)
	}

	if err := s.Start(); err != nil {
		log.Fatal(err)
	}
}
