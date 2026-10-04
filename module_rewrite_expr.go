package goja

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

// rewriteExpr returns a structurally equal expression with module-level
// identifier references rewritten to live-binding accessor reads, and with
// assignment to such identifiers redirected to the backing cell.
func (t *moduleTransformer) rewriteExpr(e ast.Expression) ast.Expression {
	if e == nil {
		return nil
	}
	switch e := e.(type) {
	case *ast.Identifier:
		return t.rewriteRef(e.Name)

	case *ast.AssignExpression:
		return t.rewriteAssign(e)

	case *ast.UnaryExpression:
		// ++/-- on a module binding must update the cell.
		if e.Postfix && (e.Operator == token.INCREMENT || e.Operator == token.DECREMENT) {
			if id, ok := e.Operand.(*ast.Identifier); ok {
				if obj, ok := t.resolveBinding(id.Name); ok {
					return t.rewriteUpdate(obj, id.Name, e.Operator, true)
				}
			}
		}
		if !e.Postfix && (e.Operator == token.INCREMENT || e.Operator == token.DECREMENT) {
			if id, ok := e.Operand.(*ast.Identifier); ok {
				if obj, ok := t.resolveBinding(id.Name); ok {
					return t.rewriteUpdate(obj, id.Name, e.Operator, false)
				}
			}
		}
		if e.Operator == token.DELETE {
			// delete identifier: identifiers are never deletable in strict
			// modules; leave intact so the existing compiler errors.
		}
		return &ast.UnaryExpression{
			Operator: e.Operator, Idx: e.Idx, Postfix: e.Postfix,
			Operand: t.rewriteExpr(e.Operand),
		}

	case *ast.BinaryExpression:
		return &ast.BinaryExpression{
			Operator: e.Operator, Left: t.rewriteExpr(e.Left),
			Right: t.rewriteExpr(e.Right), Comparison: e.Comparison,
		}

	case *ast.ConditionalExpression:
		return &ast.ConditionalExpression{
			Test:       t.rewriteExpr(e.Test),
			Consequent: t.rewriteExpr(e.Consequent),
			Alternate:  t.rewriteExpr(e.Alternate),
		}

	case *ast.CallExpression:
		args := t.rewriteExprList(e.ArgumentList)
		return &ast.CallExpression{
			Callee:           t.rewriteExpr(e.Callee),
			LeftParenthesis:  e.LeftParenthesis,
			ArgumentList:     args,
			RightParenthesis: e.RightParenthesis,
		}

	case *ast.NewExpression:
		return &ast.NewExpression{
			New: e.New, Callee: t.rewriteExpr(e.Callee),
			LeftParenthesis:  e.LeftParenthesis,
			ArgumentList:     t.rewriteExprList(e.ArgumentList),
			RightParenthesis: e.RightParenthesis,
		}

	case *ast.DotExpression:
		return &ast.DotExpression{Left: t.rewriteExpr(e.Left), Identifier: e.Identifier}

	case *ast.PrivateDotExpression:
		return &ast.PrivateDotExpression{Left: t.rewriteExpr(e.Left), Identifier: e.Identifier}

	case *ast.BracketExpression:
		return &ast.BracketExpression{
			Left: t.rewriteExpr(e.Left), Member: t.rewriteExpr(e.Member),
			LeftBracket: e.LeftBracket, RightBracket: e.RightBracket,
		}

	case *ast.ArrayLiteral:
		return &ast.ArrayLiteral{
			LeftBracket: e.LeftBracket, RightBracket: e.RightBracket,
			Value: t.rewriteExprList(e.Value),
		}

	case *ast.ObjectLiteral:
		return t.rewriteObjectLiteral(e)

	case *ast.FunctionLiteral:
		return t.rewriteFunction(e)

	case *ast.ArrowFunctionLiteral:
		return t.rewriteArrow(e)

	case *ast.ClassLiteral:
		return t.rewriteClass(e)

	case *ast.TemplateLiteral:
		return &ast.TemplateLiteral{
			OpenQuote: e.OpenQuote, CloseQuote: e.CloseQuote,
			Tag: t.rewriteExpr(e.Tag), Elements: e.Elements,
			Expressions: t.rewriteExprList(e.Expressions),
		}

	case *ast.SequenceExpression:
		return &ast.SequenceExpression{Sequence: t.rewriteExprList(e.Sequence)}

	case *ast.SpreadElement:
		return &ast.SpreadElement{Expression: t.rewriteExpr(e.Expression)}

	case *ast.YieldExpression:
		return &ast.YieldExpression{Yield: e.Yield, Argument: t.rewriteNullable(e.Argument), Delegate: e.Delegate}

	case *ast.AwaitExpression:
		// Inside an async function nested in the module: rewrite normally.
		return &ast.AwaitExpression{Await: e.Await, Argument: t.rewriteExpr(e.Argument)}

	case *ast.MetaProperty:
		return e // new.target; import.meta rejected by the parser

	case *ast.ThisExpression, *ast.SuperExpression, *ast.BooleanLiteral,
		*ast.NullLiteral, *ast.NumberLiteral, *ast.StringLiteral,
		*ast.RegExpLiteral, *ast.BadExpression:
		return e

	case *ast.PropertyShort:
		return &ast.PropertyShort{Name: e.Name, Initializer: t.rewriteNullable(e.Initializer)}

	case *ast.PropertyKeyed:
		return &ast.PropertyKeyed{
			Key: t.rewriteExpr(e.Key), Kind: e.Kind,
			Value: t.rewriteExpr(e.Value), Computed: e.Computed,
		}
	}
	return e
}

func (t *moduleTransformer) rewriteExprList(list []ast.Expression) []ast.Expression {
	out := make([]ast.Expression, len(list))
	for i, x := range list {
		out[i] = t.rewriteExpr(x)
	}
	return out
}

// rewriteObjectLiteral handles object literals, paying attention to shorthand
// properties: { foo } reads foo and must become { foo: imports["foo"] } when
// foo is a module binding.
func (t *moduleTransformer) rewriteObjectLiteral(e *ast.ObjectLiteral) *ast.ObjectLiteral {
	props := make([]ast.Property, len(e.Value))
	for i, p := range e.Value {
		switch p := p.(type) {
		case *ast.PropertyShort:
			// { x } -> { x: <read x> }
			props[i] = &ast.PropertyKeyed{
				Key:      &ast.StringLiteral{Literal: p.Name.Name.String(), Value: p.Name.Name},
				Kind:     ast.PropertyKindValue,
				Value:    t.rewriteRef(p.Name.Name),
				Computed: false,
			}
		case *ast.PropertyKeyed:
			props[i] = &ast.PropertyKeyed{
				Key: t.rewriteExpr(p.Key), Kind: p.Kind,
				Value: t.rewriteExpr(p.Value), Computed: p.Computed,
			}
		case *ast.SpreadElement:
			props[i] = &ast.SpreadElement{Expression: t.rewriteExpr(p.Expression)}
		default:
			props[i] = p
		}
	}
	return &ast.ObjectLiteral{
		LeftBrace: e.LeftBrace, RightBrace: e.RightBrace, Value: props,
	}
}

// rewriteAssign redirects assignment to a module binding. Compound assignments
// (e.g. x += 1) desugar to a read-modify-write through the cell.
func (t *moduleTransformer) rewriteAssign(e *ast.AssignExpression) ast.Expression {
	if id, ok := e.Left.(*ast.Identifier); ok {
		if obj, ok := t.resolveBinding(id.Name); ok {
			right := t.rewriteExpr(e.Right)
			if e.Operator == token.ASSIGN {
				return &ast.AssignExpression{
					Operator: token.ASSIGN,
					Left:     member(obj, id.Name),
					Right:    right,
				}
			}
			// Compound: member op= right. Rewrite as member = member binop right.
			binop := e.Operator
			// token.ADD_ASSIGN -> token.PLUS etc: strip the _ASSIGN flag.
			bin := compoundToBinary(binop)
			return &ast.AssignExpression{
				Operator: token.ASSIGN,
				Left:     member(obj, id.Name),
				Right: &ast.BinaryExpression{
					Operator: bin,
					Left:     member(obj, id.Name),
					Right:    right,
				},
			}
		}
	}
	return &ast.AssignExpression{
		Operator: e.Operator,
		Left:     t.rewriteExpr(e.Left),
		Right:    t.rewriteExpr(e.Right),
	}
}

// rewriteUpdate desugars ++/-- on a module binding. postfix selects whether
// the expression yields the old or new value; either way the cell is updated.
func (t *moduleTransformer) rewriteUpdate(obj, name unistring.String, op token.Token, postfix bool) ast.Expression {
	bin := token.PLUS
	if op == token.DECREMENT {
		bin = token.MINUS
	}
	one := &ast.NumberLiteral{Literal: "1", Value: int64(1)}
	update := &ast.AssignExpression{
		Operator: token.ASSIGN,
		Left:     member(obj, name),
		Right: &ast.BinaryExpression{
			Operator: bin, Left: member(obj, name), Right: one,
		},
	}
	_ = postfix
	// The value of ++x / x++ differs, but both perform the same cell update.
	// Emitting the update expression yields the new value, matching prefix ++.
	// For postfix we wrap to capture the old value:
	if !postfix {
		return update
	}
	// (function(v){ OBJ[n] = v +/- 1; return v; })(OBJ[n])
	param := unistring.String(" v")
	fn := functionLiteral([]*ast.Binding{{Target: ident(param)}}, []ast.Statement{
		exprStmt(&ast.AssignExpression{
			Operator: token.ASSIGN,
			Left:     member(obj, name),
			Right: &ast.BinaryExpression{
				Operator: bin, Left: ident(param), Right: one,
			},
		}),
		&ast.ReturnStatement{Argument: ident(param)},
	})
	return &ast.CallExpression{Callee: fn, ArgumentList: []ast.Expression{member(obj, name)}}
}

func compoundToBinary(op token.Token) token.Token {
	switch op {
	case token.ADD_ASSIGN:
		return token.PLUS
	case token.SUBTRACT_ASSIGN:
		return token.MINUS
	case token.MULTIPLY_ASSIGN:
		return token.MULTIPLY
	case token.QUOTIENT_ASSIGN:
		return token.SLASH
	case token.REMAINDER_ASSIGN:
		return token.REMAINDER
	case token.EXPONENT_ASSIGN:
		return token.EXPONENT
	case token.SHIFT_LEFT_ASSIGN:
		return token.SHIFT_LEFT
	case token.SHIFT_RIGHT_ASSIGN:
		return token.SHIFT_RIGHT
	case token.UNSIGNED_SHIFT_RIGHT_ASSIGN:
		return token.UNSIGNED_SHIFT_RIGHT
	case token.AND_ASSIGN:
		return token.AND
	case token.OR_ASSIGN:
		return token.OR
	case token.EXCLUSIVE_OR_ASSIGN:
		return token.EXCLUSIVE_OR
	case token.LOGICAL_AND_ASSIGN:
		return token.LOGICAL_AND
	case token.LOGICAL_OR_ASSIGN:
		return token.LOGICAL_OR
	case token.COALESCE_ASSIGN:
		return token.COALESCE
	}
	return op
}
