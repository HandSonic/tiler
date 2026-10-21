package main

// TileMap 瓦片地图类型
type TileMap struct {
	ID          int
	Name        string
	Description string
	Schema      string //no types,maybe "xyz" or "tms"
	Min         int
	Max         int
	Format      string
	JSON        string
	URL         string
	Token       string
	//such as porxy...
}
