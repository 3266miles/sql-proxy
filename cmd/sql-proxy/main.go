package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"

	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
)

type City struct {
	ID   int
	Name string
}

func main() {
	if err := realMain(); err != nil {
		log.Fatalln(err)
	}
}

func realMain() error {
	user := flag.String("user", "root", "MySQL user")
	password := flag.String("password", "", "MySQL password")
	addr := flag.String("addr", "", "MySQL network address")
	dbname := flag.String("db", "", "MySQL Database name")

	flag.Parse()

	fmt.Printf("*addr = %+v\n", *addr)

	cfg := mysql.NewConfig()
	cfg.User = *user
	cfg.Passwd = *password
	cfg.Net = "tcp"
	cfg.Addr = *addr
	cfg.DBName = *dbname

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	err = db.Ping() // check that connection is working
	if err != nil {
		return err
	}

	rows, err := db.Query("SELECT * FROM cities")
	defer rows.Close()
	if err != nil {
		return err
	}

	for rows.Next() {
		var city City
		err := rows.Scan(&city.ID, &city.Name)
		if err != nil {
			return err
		}

		fmt.Printf("%v\n", city)
	}

	if err := rows.Err(); err != nil {
		return err
	}

	return nil
}