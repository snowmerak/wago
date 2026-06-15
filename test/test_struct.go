package main

import (
	"errors"
)

//go:generate go run ../main.go

type User struct {
	Name       string
	Age        int
	Active     bool
	Nickname   *string
	Profile    Profile
	Address    *Address
	Tags       []string
	Friends    []Friend
	Metadata   map[string]string
	BestFriend *Friend
	AllFriends []*Friend
	Scores     map[string]*int
}

type Profile struct {
	Bio string
}

type Address struct {
	City string
}

type Friend struct {
	Name string
}

//wago:export
func ProcessUser(u User) string {
	return "Processed: " + u.Name
}

//wago:export async
func ProcessUserAsync(u User) (User, error) {
	if u.Age < 0 {
		return User{}, errors.New("age cannot be negative")
	}
	u.Name = "Async: " + u.Name
	return u, nil
}

//wago:export
func ProcessBytes(data []byte) []byte {
	out := make([]byte, len(data))
	for i, v := range data {
		out[i] = v + 1
	}
	return out
}

//wago:export
func GetComplexUser() User {
	nickname := "snow"
	score1 := 100
	score2 := 90
	return User{
		Name:     "Snowmerak",
		Age:      30,
		Active:   true,
		Nickname: &nickname,
		Profile:  Profile{Bio: "Software Engineer"},
		Address:  &Address{City: "Seoul"},
		Tags:     []string{"go", "wasm"},
		Friends:  []Friend{{Name: "Alice"}, {Name: "Bob"}},
		Metadata: map[string]string{"env": "production"},
		BestFriend: &Friend{Name: "Charlie"},
		AllFriends: []*Friend{{Name: "Dave"}, nil, {Name: "Eve"}},
		Scores:     map[string]*int{"math": &score1, "science": &score2, "history": nil},
	}
}

