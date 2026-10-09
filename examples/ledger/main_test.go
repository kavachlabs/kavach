package main

import (
	"os"
	"testing"

	"github.com/kavachlabs/kavach/internal/testrecorder"
)

func TestMain(m *testing.M) { os.Exit(testrecorder.Run(m)) }
