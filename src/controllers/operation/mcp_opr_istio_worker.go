package operation

import (
	"time"
	"github.com/astaxie/beego"
	"initial"
	"models"
	"library/common"
	"library/mcp"
	"github.com/astaxie/beego/httplib"
)

// istio升级进程
type IstioUpgradeWithImage struct {
	Opr      mcp.McpIstioOpr   // 基础配置
	UnitId   int            // 更新的发布单元
	Image    string         // 更新后的镜像
	OldImage string
	Operator string         // 操作人员
	RecordId   int            // 操作记录ID
	SourceId   string         // 外部来源id
	ParentId   int          // 内部关联id，和部署主记录关联
}

func (self *IstioUpgradeWithImage) Do() {
	defer func() {
		if err := recover(); err != nil {
			beego.Error("Mcp Istio Upgrade Panic error:", err)
		}
	}()
	timeout := time.After(20 * time.Minute)
	run_env := beego.AppConfig.String("runmode")
	if run_env != "prd" {
		// 测试环境缩容器更新短超时时间
		timeout = time.After(8 * time.Minute)
	}
	result_ch := make(chan bool, 1)
	go func() {
		result := self.UpgradeImage()
		result_ch <- result
	}()
	select {
	case <-result_ch:
		beego.Info("执行完成")
		time.Sleep(1*time.Second)
		self.UpdateExecResult()
	case <-timeout:
		beego.Info("执行超时")
		self.SaveExecResult(false, run_env + "环境执行超时，容器状态异常，请上服务治理平台查看", 20 * 60)
		time.Sleep(1*time.Second)
		self.UpdateExecResult()
	}
}

func (self *IstioUpgradeWithImage) UpgradeImage() bool {
	id := self.InsertRecord()
	if id == 0 {
		beego.Error("istio升级数据录入失败！")
		return false
	}
	self.RecordId = id
	// 关联外部表
	self.RelRecord()

	start := time.Now()
	err := self.Opr.UpgradeIstioService(self.Image)
	if err != nil {
		beego.Info(err.Error())
		self.SaveExecResult(false, err.Error(), 0)
		return false
	}
	// 升级是瞬时操作，瞬时返回结果都是成功的，要等一会
	time.Sleep(60 * time.Second)
	ec := 0
	for {
		ec += 1
		if ec > 50 {
			// 设置执行次数
			cost_time := time.Now().Sub(start).Seconds()
			self.SaveExecResult(false, "执行超时，容器状态异常，请上容器平台检查！", common.GetInt(cost_time))
			return false
		}
		beego.Info("还未升级完成，请等待20秒。。。")
		time.Sleep(20 * time.Second)
		err, status := self.Opr.GetIstioStatus()
		if err != nil {
			beego.Info(err.Error())
			self.SaveExecResult(false, err.Error(), 0)
			return false
		}
		if status.Spec.Replicas == status.Status.Replicas && status.Spec.Replicas == status.Status.UpdatedReplicas {
			// 升级完成
			break
		}
	}
	cost_time := time.Now().Sub(start).Seconds()
	self.SaveExecResult(true, self.Opr.Deployment + "-" + self.Opr.Version + "镜像更新成功！", common.GetInt(cost_time))

	return true
}

func (self *IstioUpgradeWithImage) SaveExecResult(result bool, msg string, cost int) {
	int_result := 0
	if result {
		int_result = 1
	}
	update_map := map[string]interface{}{
		"result": int_result,
		"message": msg,
		"cost_time": cost,
	}
	tx := initial.DB.Begin()
	err := tx.Model(models.McpUpgradeList{}).Where("id=?", self.RecordId).Updates(update_map).Error
	if err != nil {
		beego.Error(err.Error())
		tx.Rollback()
		return
	}
	tx.Commit()
}

func (self *IstioUpgradeWithImage) InsertRecord() int {
	var record models.McpUpgradeList
	record.UnitId = self.UnitId
	record.OldImage = self.OldImage
	record.NewImage = self.Image
	record.Result = 2
	record.Operator = self.Operator
	now := time.Now()
	today := now.Format(initial.DateFormat)
	if now.Hour() < 4 {
		today = now.AddDate(0, 0, -1).Format(initial.DateFormat)
	}
	record.OnlineDate = today
	record.CostTime = 0
	record.InsertTime = now.Format(initial.DatetimeFormat)
	record.SourceId = self.SourceId
	tx := initial.DB.Begin()
	err := tx.Create(&record).Error
	if err != nil {
		beego.Error(err.Error())
		tx.Rollback()
		return 0
	}
	tx.Commit()
	return record.Id
}

// 关联到升级父表
func (self *IstioUpgradeWithImage) RelRecord() {
	if self.ParentId > 0 {
		tx := initial.DB.Begin()
		err := tx.Model(models.OnlineStdCntr{}).Where("id=?", self.ParentId).Update("opr_cntr_id", self.RecordId).Error
		if err != nil {
			beego.Error(err.Error())
			tx.Rollback()
			return
		}
		tx.Commit()
	}
}

// 结果返回到pms或者更新到主表
func (self *IstioUpgradeWithImage) UpdateExecResult() {
	if self.ParentId > 0 {
		// 更新主表
		tx := initial.DB.Begin()
		var sub_info models.OnlineStdCntr
		err := tx.Model(models.OnlineStdCntr{}).Where("id=?", self.ParentId).First(&sub_info).Error
		if err != nil {
			beego.Error(err.Error())
			tx.Rollback()
			return
		}

		var opr models.McpUpgradeList
		err = tx.Model(models.McpUpgradeList{}).Where("id=?", self.RecordId).First(&opr).Error
		if err != nil {
			beego.Error(err.Error())
			tx.Rollback()
			return
		}
		update_map := map[string]interface{}{
			"is_success": opr.Result,
			"error_log": opr.Message,
		}
		err = tx.Model(models.OnlineAllList{}).Where("id=?", sub_info.OnlineId).Updates(update_map).Error
		if err != nil {
			beego.Error(err.Error())
			tx.Rollback()
			return
		}
		tx.Commit()

		// 回推发布管理系统。通过发布管理系统拉取时，只更新主表的source_id即可。
		// 升级子表的source_id，用于cpds或者devops直接调用。两者不宜混淆
		var record models.OnlineAllList
		err = initial.DB.Model(models.OnlineAllList{}).Where("id=?", sub_info.OnlineId).First(&record).Error
		if err != nil {
			beego.Error(err.Error())
			return
		}
		if record.SourceId != "" && record.SourceId != "0" {
			req := httplib.Get(beego.AppConfig.String("pms_baseurl") + "/mdp/release/result")
			req.Header("Authorization", "Basic mdeploy_d8c8680d046b1c60e63657deb3ce6d89")
			req.Header("Content-Type", "application/json")
			req.Param("record_id", record.SourceId)
			req.Param("result", common.GetString(opr.Result))
			_, err := req.String()
			if err != nil {
				beego.Error(err.Error())
			}
		}
	}
}