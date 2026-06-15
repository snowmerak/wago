package main

//go:generate go run ../main.go -type=User,Profile,Address,Friend

type User struct {
	Name     string
	Age      int
	Active   bool
	Nickname *string
	Profile  Profile
	Address  *Address
	Tags     []string
	Friends  []Friend
	Metadata map[string]string
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

func main() {}
