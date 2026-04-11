//nolint:revive
package names

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
)

type nameGenerator struct {
	firstNames []string
	lastNames  []string
}

func (n *nameGenerator) loadNames(path string) ([]string, error) {
	//nolint:gosec
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open names file: %w", err)
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

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan names file: %w", err)
	}
	return names, nil
}

func (n *nameGenerator) loadNameFiles(firstPath, lastPath string) {
	if names, err := n.loadNames(firstPath); err == nil {
		n.firstNames = names
	}

	if names, err := n.loadNames(lastPath); err == nil {
		n.lastNames = names
	}
}

func (n *nameGenerator) generate() string {
	first := n.firstNames[secureRandInt(len(n.firstNames))]
	last := n.lastNames[secureRandInt(len(n.lastNames))]
	return first + " " + last
}

//nolint:revive
func secureRandInt(n int) int {
	if n <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	val := binary.BigEndian.Uint64(b[:]) % uint64(n)
	if val > math.MaxInt {
		return 0
	}
	return int(val)
}

func getDefaultGenerator() *nameGenerator {
	getGeneratorOnce().Do(func() {
		inst := getGeneratorInstance()
		inst.firstNames = []string{
			"Александр", "Дмитрий", "Максим", "Сергей", "Андрей", "Алексей", "Артём", "Илья", "Кирилл", "Михаил",
			"Никита", "Матвей", "Роман", "Егор", "Арсений", "Иван", "Денис", "Евгений", "Даниил", "Тимофей",
			"Владислав", "Игорь", "Владимир", "Павел", "Руслан", "Марк", "Константин", "Николай", "Олег", "Виктор",
		}
		inst.lastNames = []string{
			"Иванов", "Смирнов", "Кузнецов", "Попов", "Васильев", "Петров", "Соколов", "Михайлов", "Новиков", "Фёдоров",
			"Морозов", "Волков", "Алексеев", "Лебедев", "Семёнов", "Егоров", "Павлов", "Козлов", "Степанов", "Николаев",
			"Орлов", "Андреев", "Макаров", "Никитин", "Захаров", "Зайцев", "Соловьёв", "Борисов", "Яковлев", "Григорьев",
		}
	})
	return getGeneratorInstance()
}

func getGeneratorInstance() *nameGenerator {
	return &generatorInstance
}

func getGeneratorOnce() *sync.Once {
	return &generatorOnce
}

//nolint:gochecknoglobals
var generatorInstance nameGenerator

//nolint:gochecknoglobals
var generatorOnce sync.Once

//nolint:revive
func LoadNameFiles(firstPath, lastPath string) error {
	getDefaultGenerator().loadNameFiles(firstPath, lastPath)
	return nil
}

//nolint:revive
func Generate() string {
	return getDefaultGenerator().generate()
}
