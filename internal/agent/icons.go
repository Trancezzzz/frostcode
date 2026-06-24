package agent

// Centralized glyphs. These are monochrome, text-presentation symbols (not
// color emoji) so they render consistently as "icons" across terminals.
const (
	icTool     = "●" // a tool is running
	icOK       = "✓" // success
	icErr      = "✗" // error
	icInfo     = "›" // notice
	icSub      = "└" // sub-agent activity
	icSel      = "▌" // palette selection bar
	icBullet   = "•" // list bullet
	icArrow    = "→" // model/route arrow
	icTodoDone = "✓"
	icTodoNow  = "▸"
	icTodoTodo = "○"
	icInterro  = "?" // approval prompt
	icCancel   = "■" // interrupt
)
