# PGBastion Sales Guide
## High Availability for PostgreSQL — Simplified

---

## What Is PGBastion?

PGBastion is a **single, unified solution** that keeps your customers' PostgreSQL databases running 24/7, even when servers fail.

Think of it like a smart traffic controller for databases — it automatically detects problems, redirects traffic to healthy servers, and fixes issues before anyone notices.

**The simple pitch:** *"PGBastion ensures your database never goes down, and if something breaks, it fixes itself in seconds — not hours."*

---

## The Problem We Solve

When a company's database goes down, **everything stops**:
- Websites crash
- Orders can't be processed
- Customers leave
- Revenue is lost

**The old way** to prevent this required stitching together 4-5 different tools:
- Patroni (cluster management)
- etcd (coordination)
- HAProxy (traffic routing)
- Keepalived (IP failover)
- Plus custom scripts and monitoring

This created a **maintenance nightmare** — different vendors, different configurations, different support contracts, and when something breaks, nobody knows whose problem it is.

**PGBastion replaces all of this with one product.**

---

## Why Customers Choose PGBastion

### 1. One Product, One Vendor, One Invoice
| Old Way | PGBastion |
|---------|-----------|
| 4-5 separate tools | 1 unified solution |
| 4-5 config files | 1 simple config file |
| Multiple support contracts | Single support contact |
| Finger-pointing when things break | Clear accountability |

**Sales angle:** *"Stop paying for 5 tools when one does it better."*

---

### 2. Faster Recovery = Less Downtime

| Metric | Traditional Stack | PGBastion |
|--------|-------------------|-----------|
| Failover time | 30-60 seconds | **Under 10 seconds** |
| Detection time | 10-30 seconds | **2-5 seconds** |
| Human intervention needed | Often | **Never** |

**The math:** If a customer has $100,000/hour in database-dependent revenue:
- Traditional: 1 minute downtime = $1,667 lost
- PGBastion: 10 seconds = $278 lost
- **Savings per incident: $1,389**

---

### 3. Dramatically Simpler Operations

**Before PGBastion:**
- Need specialists for each component
- Complex troubleshooting across multiple logs
- Upgrades require coordinating 5 different tools
- Training takes weeks

**With PGBastion:**
- One tool to learn
- One dashboard to monitor
- One log to check
- Training takes hours, not weeks

**Sales angle:** *"Your team will actually understand how it works."*

---

### 4. Built-In Features Others Charge Extra For

| Feature | Competitors | PGBastion |
|---------|-------------|-----------|
| Web Dashboard | Extra cost | **Included** |
| Connection Pooling | Separate tool (PgBouncer) | **Built-in** |
| Distributed Tracing | Requires setup | **Built-in** |
| Prometheus Metrics | Partial | **Complete** |
| API Access | Limited | **Full REST API** |

---

### 5. Enterprise-Grade Security

- **Authentication required** for all management operations
- **Encrypted communications** between nodes
- **Audit logging** for compliance (SOC 2, HIPAA, PCI)
- **No remote execution** — each node manages itself (reduces attack surface)

**Sales angle:** *"Built for companies that take security seriously."*

---

## Competitive Comparison

### vs. Patroni + etcd + HAProxy Stack

| Factor | Traditional Stack | PGBastion | Winner |
|--------|-------------------|-----------|--------|
| Components to manage | 4-5 | 1 | PGBastion |
| Configuration complexity | High | Low | PGBastion |
| Failover speed | 30-60s | <10s | PGBastion |
| Learning curve | Steep | Gentle | PGBastion |
| Total cost of ownership | $$$$$ | $$ | PGBastion |
| Vendor support | Fragmented | Unified | PGBastion |

### vs. Cloud-Managed Databases (AWS RDS, Azure, GCP)

| Factor | Cloud Managed | PGBastion | Winner |
|--------|---------------|-----------|--------|
| Monthly cost (large DB) | $2,000-10,000 | $500-2,000 | PGBastion |
| Data sovereignty | Vendor controls | Customer controls | PGBastion |
| Vendor lock-in | High | None | PGBastion |
| Customization | Limited | Full | PGBastion |
| Multi-cloud | No | Yes | PGBastion |

**Sales angle for cloud comparison:** *"Get cloud-level reliability without cloud-level bills or lock-in."*

---

## Ideal Customer Profile

### Best Fit:
- **Mid-size to Enterprise** companies with critical PostgreSQL databases
- **SaaS companies** where uptime directly impacts revenue
- **Financial services** with strict compliance requirements
- **Healthcare** organizations needing HIPAA compliance
- **E-commerce** businesses where every second of downtime costs money
- **Companies leaving the cloud** to reduce costs or regain control

### Trigger Events (When to Reach Out):
- Recent database outage or near-miss
- Cloud bill shock — looking to reduce costs
- Compliance audit coming up
- Migrating from Oracle/SQL Server to PostgreSQL
- Current HA solution causing problems
- Team struggling to maintain complex stack

---

## Enterprise Pricing

### PGBastion Editions

| Edition | Starter | Professional | Enterprise |
|---------|---------|--------------|------------|
| **Target** | Small teams | Growing companies | Large organizations |
| **Nodes** | Up to 3 | Up to 10 | Unlimited |
| **Support** | Email (48hr) | Email + Phone (24hr) | 24/7 + Dedicated CSM |
| **SLA** | 99.5% | 99.9% | 99.99% |
| **Features** | Core HA | + Connection Pooling, Dashboard | + Tracing, Priority Patches |
| **Training** | Docs only | 2hr onboarding | Full training program |
| | | | |
| **Annual Price** | **$6,000/yr** | **$18,000/yr** | **$48,000/yr** |
| *Per node/month* | *$167/node* | *$150/node* | *Custom* |

### Add-Ons

| Add-On | Price | Description |
|--------|-------|-------------|
| Premium Support | $12,000/yr | 1-hour response, direct engineer access |
| Professional Services | $2,500/day | Custom implementation, migration assistance |
| Training Package | $5,000 | Full-day on-site or virtual training |
| Health Check | $3,500 | Architecture review + recommendations |

### Volume Discounts
- 10+ nodes: 10% off
- 25+ nodes: 15% off
- 50+ nodes: 20% off
- Multi-year contracts: Additional 10% off

---

## ROI Calculator (Use With Customers)

### Quick ROI Math:

**Downtime Cost:**
- Customer's hourly revenue dependent on database: $________
- Average incidents per year: ________ (industry avg: 4-6)
- Average downtime per incident (current): ________ minutes

**Current annual downtime cost:** Revenue x Incidents x (Minutes/60) = $________

**With PGBastion:**
- Reduce downtime by 80%+
- **Annual savings:** $________

**Operational Savings:**
- Hours/month managing current stack: ________
- Fully-loaded engineer cost/hour: $________ (avg: $100-150)
- **Annual operational savings:** Hours x 12 x Cost = $________

**Total Annual Value:** Downtime savings + Operational savings = $________

**PGBastion Cost:** $________

**Net ROI:** (Value - Cost) / Cost = ________%

*Typical customers see 300-500% ROI in year one.*

---

## Common Objections & Responses

### "We already have Patroni working fine."

*"That's great — Patroni is solid software. But let me ask: how many hours per month does your team spend maintaining the full stack? And when was your last failover test? Many teams we talk to haven't tested in months because it's too complex. PGBastion makes testing so simple you can do it weekly."*

### "We're moving to the cloud anyway."

*"Cloud databases are convenient, but have you seen the pricing at scale? We have customers who saved 60-70% by running PostgreSQL on their own infrastructure with PGBastion. Plus, you keep full control of your data and avoid vendor lock-in."*

### "We can't afford downtime during migration."

*"Totally understand. We offer a parallel deployment option — run PGBastion alongside your current stack, verify it works perfectly, then cut over with zero downtime. Our Professional Services team has done this hundreds of times."*

### "Our team doesn't have time to learn something new."

*"That's actually the best reason to switch. PGBastion is dramatically simpler than managing 4-5 separate tools. Most teams are fully productive in a day or two. And our Enterprise plan includes full training."*

### "It's not in the budget."

*"Let's calculate the cost of your last outage — or even a near-miss that required weekend work. When you factor in downtime costs and operational overhead, PGBastion typically pays for itself within 3-6 months."*

---

## Proof Points & Social Proof

### Case Studies (Example Templates)

**FinTech Company:**
> "After our third database incident in 6 months, we knew we needed a change. PGBastion reduced our failover time from 45 seconds to 8 seconds, and we haven't had an unplanned outage since deployment."
> — VP of Engineering

**E-Commerce Platform:**
> "We were spending $15,000/month on cloud database services. Migrating to self-managed PostgreSQL with PGBastion cut that to $4,000/month while improving our reliability."
> — CTO

**Healthcare SaaS:**
> "For HIPAA compliance, we needed to prove our database HA solution was secure and auditable. PGBastion's built-in security and logging made our audit a breeze."
> — Compliance Officer

---

## Discovery Questions

Use these to uncover pain and qualify opportunities:

1. **"What happens when your database goes down?"** (Understand impact)

2. **"How long does it take to recover from a database failure?"** (Current state)

3. **"How many people are involved in managing your database infrastructure?"** (Operational burden)

4. **"When was your last failover test?"** (Often reveals lack of confidence)

5. **"What's your current monthly spend on database infrastructure?"** (Budget context)

6. **"Are you planning any database migrations in the next 12 months?"** (Timing)

7. **"Who else would need to be involved in a decision like this?"** (Buying committee)

---

## Quick Reference Card

### Elevator Pitch (30 seconds)
*"PGBastion is enterprise PostgreSQL high availability in a single product. It replaces the 4-5 tools companies typically stitch together, cuts failover time from minutes to seconds, and dramatically simplifies operations. Our customers typically see 300%+ ROI in the first year from reduced downtime and operational savings."*

### Key Differentiators
1. **One product** replaces 4-5 tools
2. **10x faster** failover than alternatives
3. **80% less** operational complexity
4. **Built-in** pooling, dashboard, and tracing
5. **Enterprise security** out of the box

### Ideal Deal Size
- Starter: $6K/year (1-3 nodes)
- Professional: $18K/year (4-10 nodes)
- Enterprise: $48K+/year (10+ nodes)

### Sales Cycle
- Starter: 2-4 weeks
- Professional: 4-8 weeks
- Enterprise: 8-16 weeks

---

## Next Steps Checklist

After initial meeting:
- [ ] Send ROI calculator worksheet
- [ ] Schedule technical deep-dive with their DBA/Engineers
- [ ] Provide trial license (14 days)
- [ ] Connect them with reference customer
- [ ] Schedule follow-up in 1 week

---

*For technical questions or demo support, contact: solutions@pgbastion.io*

*Last updated: February 2026*
