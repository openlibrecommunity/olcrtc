package names

import (
	"bufio"
	"log"
	"math/rand/v2"
	"os"
	"strings"
)

var (
	firstNames []string
	lastNames  []string
)

var defaultFirstNames = []string{
	"Александр", "Дмитрий", "Максим", "Сергей", "Андрей", "Алексей", "Артём", "Илья", "Кирилл", "Михаил",
	"Никита", "Матвей", "Роман", "Егор", "Арсений", "Иван", "Денис", "Евгений", "Даниил", "Тимофей",
	"Владислав", "Игорь", "Владимир", "Павел", "Руслан", "Марк", "Константин", "Николай", "Олег", "Виктор",
}

var defaultLastNames = []string{
	"Иванов", "Смирнов", "Кузнецов", "Попов", "Васильев", "Петров", "Соколов", "Михайлов", "Новиков", "Фёдоров",
	"Морозов", "Волков", "Алексеев", "Лебедев", "Семёнов", "Егоров", "Павлов", "Козлов", "Степанов", "Николаев",
	"Орлов", "Андреев", "Макаров", "Никитин", "Захаров", "Зайцев", "Соловьёв", "Борисов", "Яковлев", "Григорьев",
}

func loadNames(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("Error closing file: %v", err)
		}
	}()

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
	firstNames = defaultFirstNames
	lastNames = defaultLastNames

	if names, err := loadNames(firstPath); err == nil {
		firstNames = names
	}

	if names, err := loadNames(lastPath); err == nil {
		lastNames = names
	}

	return nil
}

func Generate() string {
	first := firstNames[rand.IntN(len(firstNames))]
	last := lastNames[rand.IntN(len(lastNames))]

	return first + " " + last
}
