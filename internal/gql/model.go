package gql

// Robot は GraphQL の Robot 型に対応する内部モデル。
//
// name と commands は「わざと」ここに持たせない。gqlgen にフィールドリゾルバを
// 生成させ、要求されたときだけ引くようにするため:
//   - name    → 別表 robot_profile を引く（射影・EXP-19）。一覧で name を要求しなければ引かない。
//   - commands → そのロボットの命令を引く（ネスト＝N+1 の温床。DataLoader で畳む・EXP-23）。
type Robot struct {
	ID      string
	Status  RobotStatus
	Battery int
	Online  bool
}
