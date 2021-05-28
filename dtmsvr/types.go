package dtmsvr

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/yedf/dtm/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type M = map[string]interface{}

var p2e = common.P2E
var e2p = common.E2P

type TransGlobal struct {
	common.ModelBase
	Gid           string `json:"gid"`
	TransType     string `json:"trans_type"`
	Data          string `json:"data"`
	Status        string `json:"status"`
	QueryPrepared string `json:"query_prepared"`
	CommitTime    *time.Time
	FinishTime    *time.Time
	RollbackTime  *time.Time
}

func (*TransGlobal) TableName() string {
	return "trans_global"
}

func (t *TransGlobal) touch(db *common.MyDb) *gorm.DB {
	writeTransLog(t.Gid, "touch trans", "", "", "")
	return db.Model(&TransGlobal{}).Where("gid=?", t.Gid).Update("gid", t.Gid) // 更新update_time，避免被定时任务再次
}

func (t *TransGlobal) changeStatus(db *common.MyDb, status string) *gorm.DB {
	writeTransLog(t.Gid, "change status", status, "", "")
	updates := M{
		"status": status,
	}
	if status == "succeed" {
		updates["finish_time"] = time.Now()
	} else if status == "failed" {
		updates["rollback_time"] = time.Now()
	}
	dbr := db.Must().Model(t).Where("status=?", t.Status).Updates(updates)
	checkAffected(dbr)
	t.Status = status
	return dbr
}

type TransBranch struct {
	common.ModelBase
	Gid          string
	Url          string
	Data         string
	Branch       string
	BranchType   string
	Status       string
	FinishTime   *time.Time
	RollbackTime *time.Time
}

func (*TransBranch) TableName() string {
	return "trans_branch"
}

func (t *TransBranch) changeStatus(db *common.MyDb, status string) *gorm.DB {
	writeTransLog(t.Gid, "step change", status, t.Branch, "")
	dbr := db.Must().Model(t).Where("status=?", t.Status).Updates(M{
		"status":      status,
		"finish_time": time.Now(),
	})
	checkAffected(dbr)
	t.Status = status
	return dbr
}

func checkAffected(db1 *gorm.DB) {
	if db1.RowsAffected == 0 {
		panic(fmt.Errorf("duplicate updating"))
	}
}

func (trans *TransGlobal) getProcessor() TransProcessor {
	if trans.TransType == "saga" {
		return &TransSagaProcessor{TransGlobal: trans}
	} else if trans.TransType == "tcc" {
		return &TransTccProcessor{TransGlobal: trans}
	} else if trans.TransType == "xa" {
		return &TransXaProcessor{TransGlobal: trans}
	}
	return nil
}

func (trans *TransGlobal) Process(db *common.MyDb) {
	defer handlePanic()
	defer func() {
		if TransProcessedTestChan != nil {
			TransProcessedTestChan <- trans.Gid
		}
	}()
	branches := []TransBranch{}
	db.Must().Where("gid=?", trans.Gid).Order("id asc").Find(&branches)
	trans.getProcessor().ProcessOnce(db, branches)
}

func (t *TransGlobal) SaveNew(db *common.MyDb) {
	err := db.Transaction(func(db1 *gorm.DB) error {
		db := &common.MyDb{DB: db1}

		writeTransLog(t.Gid, "create trans", t.Status, "", t.Data)
		dbr := db.Must().Clauses(clause.OnConflict{
			DoNothing: true,
		}).Create(t)
		if dbr.RowsAffected == 0 && t.Status == "committed" { // 如果数据库已经存放了prepared的事务，则修改状态
			dbr = db.Must().Model(&TransGlobal{}).Where("gid=? and status=?", t.Gid, "prepared").Update("status", t.Status)
		}
		if dbr.RowsAffected == 0 { // 未保存任何数据，直接返回
			return nil
		}
		// 保存所有的分支
		nsteps := t.getProcessor().GenBranches()
		if len(nsteps) > 0 {
			writeTransLog(t.Gid, "save steps", t.Status, "", common.MustMarshalString(nsteps))
			db.Must().Clauses(clause.OnConflict{
				DoNothing: true,
			}).Create(&nsteps)
		}
		return nil
	})
	e2p(err)
}

func TransFromContext(c *gin.Context) *TransGlobal {
	data := M{}
	b, err := c.GetRawData()
	e2p(err)
	common.MustUnmarshal(b, &data)
	logrus.Printf("creating trans in prepare")
	if data["steps"] != nil {
		data["data"] = common.MustMarshalString(data["steps"])
	}
	m := TransGlobal{}
	common.MustRemarshal(data, &m)
	return &m
}

func TransFromDb(db *common.MyDb, gid string) *TransGlobal {
	m := TransGlobal{}
	dbr := db.Must().Model(&m).Where("gid=?", gid).First(&m)
	e2p(dbr.Error)
	return &m
}
