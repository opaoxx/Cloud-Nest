package main

import (
	"log"
	"os"

	"github.com/gin-gonic/gin"
)

func main() {
	listenAddress := os.Getenv("CLOUDNEST_HTTP_ADDR")
	if listenAddress == "" {
		listenAddress = "127.0.0.1:18081"
	}

	r := gin.Default()

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "CloudNest API is running",
		})
	})

	log.Printf("Server starting on %s", listenAddress)
	if err := r.Run(listenAddress); err != nil {
		log.Fatal(err)
	}
}
