package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"bitcask/src"
)

func main() {
	dbPath := "my_database.db"
	db, err := src.Open(dbPath, &src.Options{SyncPolicy: src.SyncAlways})
	if err != nil {
		fmt.Printf("Error opening database: %v\n", err)
		return
	}
	defer db.Close()

	fmt.Println("==========================================================")
	fmt.Printf("Bitcask Database CLI connected to: %s\n", dbPath)
	fmt.Println("Commands:")
	fmt.Println("  put <key> <value>   - Store a key and value")
	fmt.Println("  get <key>           - Retrieve value for a key")
	fmt.Println("  del <key>           - Delete a key")
	fmt.Println("  compact             - Run compaction to reclaim disk space")
	fmt.Println("  reopen              - Close and reopen DB (tests crash recovery)")
	fmt.Println("  exit                - Quit the program")
	fmt.Println("==========================================================")

	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("bitcask> ")
		if !scanner.Scan() {
			break
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 3)
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "put":
			if len(parts) < 3 {
				fmt.Println("Usage: put <key> <value>")
				continue
			}
			key := parts[1]
			val := parts[2]
			if err := db.Put([]byte(key), []byte(val)); err != nil {
				fmt.Printf("Error putting key: %v\n", err)
			} else {
				fmt.Printf("OK (saved '%s' => '%s')\n", key, val)
			}

		case "get":
			if len(parts) < 2 {
				fmt.Println("Usage: get <key>")
				continue
			}
			key := parts[1]
			val, err := db.Get([]byte(key))
			if err != nil {
				if errors.Is(err, src.ErrNotFound) {
					fmt.Printf("(nil) key '%s' not found\n", key)
				} else {
					fmt.Printf("Error getting key: %v\n", err)
				}
			} else {
				fmt.Printf("\"%s\"\n", string(val))
			}

		case "del", "delete":
			if len(parts) < 2 {
				fmt.Println("Usage: del <key>")
				continue
			}
			key := parts[1]
			if err := db.Delete([]byte(key)); err != nil {
				fmt.Printf("Error deleting key: %v\n", err)
			} else {
				fmt.Printf("OK (deleted '%s')\n", key)
			}

		case "compact":
			fiBefore, _ := os.Stat(dbPath)
			sizeBefore := int64(0)
			if fiBefore != nil {
				sizeBefore = fiBefore.Size()
			}

			if err := db.Compact(); err != nil {
				fmt.Printf("Error during compaction: %v\n", err)
			} else {
				fiAfter, _ := os.Stat(dbPath)
				sizeAfter := int64(0)
				if fiAfter != nil {
					sizeAfter = fiAfter.Size()
				}
				fmt.Printf("OK (compacted! File size reduced from %d to %d bytes)\n", sizeBefore, sizeAfter)
			}

		case "reopen":
			_ = db.Close()
			newDB, err := src.Open(dbPath, &src.Options{SyncPolicy: src.SyncAlways})
			if err != nil {
				fmt.Printf("Error reopening DB: %v\n", err)
				return
			}
			db = newDB
			fmt.Println("OK (database closed and reopened via Recover scan; all data preserved!)")

		case "exit", "quit":
			fmt.Println("Bye!")
			return

		default:
			fmt.Printf("Unknown command: %s. Available: put, get, del, compact, reopen, exit\n", cmd)
		}
	}
}
