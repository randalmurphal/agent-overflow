package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	appService := NewApp()

	app := application.New(application.Options{
		Name: "Agent Overflow",
		Services: []application.Service{
			application.NewService(appService),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Agent Overflow",
		Width:            1280,
		Height:           800,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
	})

	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
