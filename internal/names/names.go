package names

import (
	"bufio"
	"math/rand/v2"
	"os"
	"strings"
)

var (
	firstNames []string
	lastNames  []string
)

func loadNames(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			names = append(names, line)
		}
	}

	return names, scanner.Err()
}

func LoadNameFiles(firstPath, lastPath string) error {
	var err error
	firstNames, err = loadNames(firstPath)
	if err != nil {
		return err
	}

	lastNames, err = loadNames(lastPath)
	if err != nil {
		return err
	}

	return nil
}

func Generate() string {
	if len(firstNames) == 0 || len(lastNames) == 0 {
		return "Unknown User"
	}

	first := firstNames[rand.IntN(len(firstNames))]
	last := lastNames[rand.IntN(len(lastNames))]

	return first + " " + last
}
