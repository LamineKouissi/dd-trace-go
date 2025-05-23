// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package profiler_test

import (
	"log"

	"github.com/DataDog/dd-trace-go/v2/profiler"
)

// This example illustrates how to run (and later stop) the Datadog Profiler.
func Example() {
	err := profiler.Start(
		profiler.WithService("users-db"),
		profiler.WithEnv("staging"),
		profiler.WithTags("version:1.2.0"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer profiler.Stop()

	// ...
}
