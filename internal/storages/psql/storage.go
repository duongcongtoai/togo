//Package psql
package psql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/manabie-com/togo/internal/lock"

	"github.com/lib/pq"
	"github.com/manabie-com/togo/internal/domain"
)

type Storage struct {
	db          *sql.DB
	lock        lock.Lock
	addTaskFunc func(domain.Task, int) error
	conf        Config
	//TODO
	//consider followings for distributed lock:
	//- https://pkg.go.dev/go.etcd.io/etcd/clientv3/concurrency
	//- https://github.com/go-redsync/redsync
}

type Config struct {
	ConnString      string
	SleepOnConflict time.Duration
	RetryOnConflict int
}

//NewStorage return new psql storage
func NewStorage(c Config) (*Storage, error) {
	if c.RetryOnConflict < 1 {
		return nil, fmt.Errorf("total retry must be > 0")
	}
	db, err := sql.Open("postgres", c.ConnString)
	if err != nil {
		return nil, err
	}
	s := &Storage{
		db:   db,
		conf: c,
	}
	s.addTaskFunc = s.addTaskWithTransaction

	return s, nil
}

//WithLock Allow user to specify custom lock like etcd, redis
func (s *Storage) WithLock(l lock.Lock) {
	s.lock = l
	s.addTaskFunc = s.addTaskWithLock
}

//CleanupDB Used to cleanup test env only
func (s *Storage) CleanupDB() error {
	_, err := s.db.Exec("DELETE from tasks")
	if err != nil {
		return err
	}

	_, err = s.db.Exec("DELETE from users")
	return err
}

func (s *Storage) addTaskWithTransaction(task domain.Task, limit int) error {
	for try := 0; try < s.conf.RetryOnConflict; try++ {
		tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{
			Isolation: sql.LevelSerializable,
		})
		if err != nil {
			return err
		}

		rows := tx.QueryRow("SELECT COUNT(id) FROM tasks where tasks.user_id =$1 and tasks.created_date=$2", task.UserID, task.CreatedDate)

		var result int
		err = rows.Scan(&result)
		if err != nil {
			pgerr, ok := err.(*pq.Error)
			//serializable read conflict
			if ok && pgerr.Code == "40001" {
				time.Sleep(s.conf.SleepOnConflict)
				tx.Rollback()
				continue
			}

			tx.Rollback()
			return err
		}

		if err != nil {
			tx.Rollback()
			return err
		}
		if result >= limit {
			tx.Rollback()
			return domain.TaskLimitReached
		}
		ex := `INSERT INTO tasks(id, content, user_id, created_date) VALUES($1,$2,$3,$4)`
		_, err = tx.Exec(ex, task.ID, task.Content, task.UserID, task.CreatedDate)
		if err != nil {
			pgerr, ok := err.(*pq.Error)
			//serializable read conflict
			if ok && pgerr.Code == "40001" {
				tx.Rollback()
				time.Sleep(s.conf.SleepOnConflict)
				continue
			}

			tx.Rollback()
			return err
		}
		tx.Commit()
		return nil
	}
	return ErrTooManySerializableConflict
}

var ErrTooManySerializableConflict = errors.New("max effort resolving concurrent conflict reached")

func (s *Storage) addTaskWithLock(task domain.Task, limit int) error {
	mutex, err := s.lock.NewMutex(task.UserID)
	if err != nil {
		return err
	}
	err = mutex.Lock()
	if err != nil {
		return err
	}
	defer mutex.Unlock()
	rows, err := s.db.Query("SELECT count(id) FROM tasks where tasks.user_id =$1 and date(tasks.created_date)=current_date", task.UserID)
	if err != nil {
		return err
	}
	result := 0
	if !rows.Next() {
		return fmt.Errorf("count query received unexpected no row")
	}
	err = rows.Scan(&result)
	if err != nil {
		return fmt.Errorf("unexpected error scanning count tasks: %s", err)
	}

	if result >= limit {
		return domain.TaskLimitReached
	}
	ex := `INSERT INTO tasks(id, content, user_id, created_date) VALUES($1,$2,$3,$4)`
	_, err = s.db.Exec(ex, task.ID, task.Content, task.UserID, task.CreatedDate)
	if err != nil {
		return err
	}
	return nil
}

func (s *Storage) AddTaskWithLimitPerDay(task domain.Task, limit int) error {
	return s.addTaskFunc(task, limit)
}

func (s *Storage) GetTasksByUserIDAndDate(userID string, date string) ([]domain.Task, error) {
	rows, err := s.db.Query(
		"SELECT id,content,user_id,created_date FROM tasks where tasks.user_id =$1 and tasks.created_date=$2",
		userID, date)
	if err != nil {
		return nil, err
	}
	result := []domain.Task{}

	for rows.Next() {
		var t domain.Task
		err := rows.Scan(&t.ID, &t.Content, &t.UserID, &t.CreatedDate)
		if err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, nil
}

func (s *Storage) FindUserByID(userID string) (domain.User, error) {
	rows, err := s.db.Query("SELECT id,password,max_todo FROM users where id =$1", userID)
	empty := domain.User{}
	if err != nil {
		return empty, err
	}
	if !rows.Next() {
		return empty, domain.UserNotFound(userID)
	}
	err = rows.Scan(&empty.ID, &empty.Password, &empty.MaxTasksPerDay)
	if err != nil {
		return empty, err
	}
	return empty, nil
}

func (s *Storage) CreateUser(user domain.User) error {
	_, err := s.db.Exec("INSERT INTO users(id, password, max_todo) VALUES ($1,$2,$3)", user.ID, user.Password, user.MaxTasksPerDay)
	if err != nil {
		return err
	}
	return nil
}

func (s *Storage) GetUserTasksPerDay(userID string) (int, error) {
	rows, err := s.db.Query("SELECT max_todo FROM users where users.id =$1", userID)
	if err != nil {
		return 0, err
	}
	if !rows.Next() {
		return 0, domain.UserNotFound(userID)
	}
	var result int
	err = rows.Scan(&result)
	if err != nil {
		return 0, err
	}
	return result, nil
}
