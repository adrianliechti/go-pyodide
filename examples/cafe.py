"""A tiny cafe dashboard: terminal analytics and an Excel workbook with charts.

Run: go run ./cmd/gopy -out ./out examples/cafe.py
Wheels are loaded automatically from cafe.wheels.json.
"""

import argparse
from datetime import date, timedelta
from pathlib import Path
import random

from tabulate import tabulate
import xlsxwriter


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--days", type=int, default=30, help="days of generated sales (1–365)")
    parser.add_argument("--seed", type=int, default=7, help="seed for repeatable demo data")
    parser.add_argument("--xlsx", type=Path, default=Path("/out/cafe.xlsx"))
    args = parser.parse_args()
    if not 1 <= args.days <= 365:
        parser.error("--days must be between 1 and 365")

    # Prices and costs are in cents; all data is synthetic.
    menu = [
        ("Flat white", 550, 170),
        ("Espresso", 350, 80),
        ("Matcha latte", 650, 230),
        ("Cold brew", 500, 140),
    ]
    rng = random.Random(args.seed)
    counts = [0] * len(menu)
    daily = []
    for day in range(args.days):
        when = date(2026, 6, 1) + timedelta(days=day)
        traffic = 1.4 if when.weekday() >= 5 else 1.0
        sold = [round(rng.randint(12, 48) * traffic) for _ in menu]
        revenue = sum(n * price for n, (_, price, _) in zip(sold, menu))
        cost = sum(n * cost for n, (_, _, cost) in zip(sold, menu))
        counts = [total + n for total, n in zip(counts, sold)]
        daily.append([when.isoformat(), revenue / 100, cost / 100,
                      (revenue - cost) / 100, sum(sold)])

    products = [
        [name, n, n * price / 100, n * (price - cost) / 100]
        for n, (name, price, cost) in zip(counts, menu)
    ]
    revenue = sum(row[2] for row in products)
    profit = sum(row[3] for row in products)
    cups = sum(counts)
    levels = "▁▂▃▄▅▆▇█"
    values = [row[1] for row in daily]
    lo, hi = min(values), max(values)
    sparkline = "".join(levels[round((v - lo) / (hi - lo or 1) * 7)] for v in values)

    period = f"{args.days} day{'s' if args.days != 1 else ''}"
    print(f"\nNIGHT OWL CAFÉ  /  {period}\n")
    print(tabulate(products, headers=["Drink", "Cups", "Sales CHF", "Gross profit CHF"],
                   tablefmt="rounded_outline", intfmt=",", floatfmt=("", ",", ",.2f", ",.2f")))
    print(f"\nSales CHF {revenue:,.2f}  ·  Gross profit CHF {profit:,.2f}  ·  {cups:,} cups")
    print(f"Daily sales  {sparkline}")
    print("Generated demo data; gross profit excludes overheads.\n")

    with xlsxwriter.Workbook(str(args.xlsx)) as book:
        book.set_properties({"title": "Night Owl Café", "author": "go-pyodide"})
        title = book.add_format({"bold": True, "font_size": 26, "font_color": "#173F35"})
        money = book.add_format({"num_format": '"CHF "#,##0.00'})
        number = book.add_format({"num_format": "#,##0"})
        label = book.add_format({"bold": True, "font_color": "#58746B"})
        dashboard = book.add_worksheet("Dashboard")
        dashboard.hide_gridlines(2)
        dashboard.set_column("A:H", 15)
        dashboard.merge_range("A1:H2", "NIGHT OWL CAFÉ", title)
        dashboard.merge_range("A3:H3", f"{period} of synthetic sales · June 2026 onwards")
        for column, text, value, fmt in [
            (0, "Sales", revenue, money),
            (2, "Gross profit", profit, money),
            (4, "Cups served", cups, number),
        ]:
            dashboard.write(4, column, text, label)
            dashboard.write(5, column, value, fmt)

        sales = book.add_worksheet("Daily sales")
        sales.freeze_panes(1, 1)
        sales.set_column("A:A", 14)
        sales.set_column("B:D", 19, money)
        sales.set_column("E:E", 12, number)
        sales.add_table(0, 0, len(daily), 4, {
            "data": daily,
            "columns": [{"header": h} for h in ["Date", "Sales", "Cost", "Gross profit", "Cups"]],
            "style": "Table Style Medium 4",
        })
        sales.conditional_format(1, 1, len(daily), 1, {"type": "data_bar", "bar_color": "#3F947C"})

        drinks = book.add_worksheet("Drinks")
        drinks.set_column("A:A", 20)
        drinks.set_column("B:B", 12, number)
        drinks.set_column("C:D", 20, money)
        drinks.add_table(0, 0, len(products), 3, {
            "data": products,
            "columns": [{"header": h} for h in ["Drink", "Cups", "Sales", "Gross profit"]],
            "style": "Table Style Medium 4",
        })

        trend = book.add_chart({"type": "line"})
        for column, name, color in [(1, "Sales", "#173F35"), (3, "Gross profit", "#D89D36")]:
            trend.add_series({
                "name": name,
                "categories": ["Daily sales", 1, 0, len(daily), 0],
                "values": ["Daily sales", 1, column, len(daily), column],
                "line": {"color": color, "width": 2.5},
            })
        trend.set_title({"name": "A month at the counter" if args.days == 30 else "At the counter"})
        trend.set_y_axis({"name": "CHF", "num_format": "#,##0"})
        trend.set_legend({"position": "bottom"})
        trend.set_size({"width": 800, "height": 340})
        dashboard.insert_chart("A9", trend)

        popular = book.add_chart({"type": "bar"})
        popular.add_series({
            "categories": ["Drinks", 1, 0, len(products), 0],
            "values": ["Drinks", 1, 1, len(products), 1],
            "fill": {"color": "#3F947C"},
            "data_labels": {"value": True},
        })
        popular.set_title({"name": "What everyone is drinking"})
        popular.set_legend({"none": True})
        popular.set_size({"width": 800, "height": 300})
        dashboard.insert_chart("A27", popular)

    print(f"Created {args.xlsx} ({args.xlsx.stat().st_size:,} bytes)")


if __name__ == "__main__":
    main()
